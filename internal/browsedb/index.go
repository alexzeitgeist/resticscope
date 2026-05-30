package browsedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"resticscope/internal/model"
)

// diskCheckRows is the coarse cadence (in accepted Add calls) at which the disk
// ceiling is re-measured during indexing. The ceiling is a safety cap with
// bounded overshoot between checks, not a precise quota, so a per-node stat is
// deliberately avoided; Commit always checks once more before marking indexed.
const diskCheckRows = 10000

// errAlreadyIndexed is the path-free sentinel BeginIndex returns when a
// (repo,snapshot) is already committed. The app gates on IsIndexed under its
// session operation lock, so this is the defensive path that preserves the "no
// silent replace" contract.
var errAlreadyIndexed = errors.New("snapshot already indexed")

// nodeRow is one buffered row awaiting a batched flush.
type nodeRow struct {
	parentDID      int64
	name           string
	typ            string
	isDir          bool
	size           int64
	mtimeUnix      int64
	mtimeOffsetSec int
	mtimeKnown     bool
	perms          string
	uid, gid       uint32
	ownerKnown     bool
	linkTarget     string
	nameCI         string
}

// dirRow is one buffered directory awaiting a batched flush. parentDID is kept
// in memory only (for the bottom-up subtree-size fold at Commit); subtreeSize is
// set just before the flush and is the only one of the two persisted.
type dirRow struct {
	did         int64
	parentDID   int64
	path        string
	subtreeSize int64
}

// IndexTx is a single, terminal index transaction for one (repo, snapshot). Add
// buffers rows and flushes them in bounded multi-row batches; Commit marks the
// snapshots row indexed (sets indexed_at_unix) and commits atomically, so a
// snapshot is marked indexed only on a clean, complete pass. Once any
// Add/flush/check fails the tx is poisoned: it holds that path-free error and
// every later Add/Commit returns it, while Rollback stays safe and idempotent.
type IndexTx struct {
	db       *DB
	tx       *sql.Tx
	repo     string
	snapshot string
	sid      int64 // allocated in BeginIndex; never leaks through the app API

	buf             []nodeRow
	dirBuf          []dirRow
	insertStmt      *sql.Stmt
	dirInsertStmt   *sql.Stmt
	count           int // accepted Add calls, for progress reporting
	insertedRows    int64
	nextDID         int64
	firstDID        int64   // first did allocated in this tx (== rootDID); dids are contiguous [firstDID..nextDID-1]
	subtreeSizes    []int64 // accumulated recursive file bytes per dir, indexed by did-firstDID
	sinceDiskCheck  int
	dirs            map[string]int64
	cachedParent    string
	cachedParentDID int64
	failed          error // sticky, path-free; set on first failure
}

// BeginIndex starts an index transaction for (repo, snapshot). The caller must
// Commit on a complete stream or Rollback on cancel/error/incomplete. It allocates
// the sid in-tx (so a rolled-back run discards the reserved row → no orphans). The
// idx_nodes_dir index is left in place and maintained incrementally as rows are
// inserted — deliberately not dropped here, because a deferred post-load rebuild
// would sort the whole snapshot in the capped wasm heap and OOM.
func (db *DB) BeginIndex(ctx context.Context, repo, snapshot string) (*IndexTx, error) {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("browsedb begin-index: %w", err)
	}
	itx := &IndexTx{db: db, tx: tx, repo: repo, snapshot: snapshot}
	if err := itx.allocSID(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := itx.initNextDID(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	itx.firstDID = itx.nextDID
	itx.dirs = make(map[string]int64, 1024)
	rootDID, err := itx.ensureCleanDir(ctx, "/")
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	itx.cacheParent("/", rootDID)
	// Dir rows are held in memory until Commit (their subtree sizes are folded
	// bottom-up there), so there is no mid-stream flush here.
	return itx, nil
}

// allocSID resolves or reserves the integer sid for (repo, snapshot) inside the
// index tx, without ON CONFLICT or RETURNING (avoiding any ncruces dialect edge).
// A committed row (indexed_at_unix NOT NULL) returns errAlreadyIndexed; a leftover
// NULL reservation (shouldn't occur — a rolled-back run discards its own row) is
// reused; otherwise a fresh row is inserted and last_insert_rowid() taken as sid.
func (itx *IndexTx) allocSID(ctx context.Context) error {
	var (
		sid       int64
		indexedAt sql.NullInt64
	)
	err := itx.tx.QueryRowContext(ctx,
		`SELECT sid, indexed_at_unix FROM snapshots WHERE repo=? AND snapshot=?`,
		itx.repo, itx.snapshot).Scan(&sid, &indexedAt)
	switch {
	case err == nil:
		if indexedAt.Valid {
			return fmt.Errorf("browsedb begin-index: %w", errAlreadyIndexed)
		}
		itx.sid = sid
		return nil
	case errors.Is(err, sql.ErrNoRows):
		res, err := itx.tx.ExecContext(ctx,
			`INSERT INTO snapshots (repo,snapshot) VALUES (?,?)`, itx.repo, itx.snapshot)
		if err != nil {
			return fmt.Errorf("browsedb begin-index: %w", err)
		}
		itx.sid, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("browsedb begin-index: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("browsedb begin-index: %w", err)
	}
}

func (itx *IndexTx) initNextDID(ctx context.Context) error {
	var maxDID int64
	if err := itx.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(did), 0) FROM dirs`).Scan(&maxDID); err != nil {
		return fmt.Errorf("browsedb dir: %w", err)
	}
	itx.nextDID = maxDID + 1
	return nil
}

// ensureCleanDir resolves or reserves an interned directory path for this
// snapshot. New dirs are assigned dids in Go and written in batches to avoid a
// SELECT/INSERT wasm round-trip per unique directory.
func (itx *IndexTx) ensureCleanDir(ctx context.Context, p string) (int64, error) {
	if did, ok := itx.dirs[p]; ok {
		return did, nil
	}
	// The root's parent is 0 (no parent); every other dir records its parent did so
	// Commit can fold subtree sizes bottom-up.
	var parentDID int64
	if p != "/" {
		var err error
		if parentDID, err = itx.ensureCleanDir(ctx, path.Dir(p)); err != nil {
			return 0, err
		}
	}
	did := itx.nextDID
	itx.nextDID++
	itx.dirs[p] = did
	itx.dirBuf = append(itx.dirBuf, dirRow{did: did, parentDID: parentDID, path: p})
	// Keep the accumulator slot index equal to did-firstDID; dir rows are flushed
	// only at Commit so the buffer is never trimmed mid-stream.
	itx.subtreeSizes = append(itx.subtreeSizes, 0)
	return did, nil
}

func (itx *IndexTx) parentDID(ctx context.Context, p string) (int64, error) {
	if sameParentPath(p, itx.cachedParent) {
		return itx.cachedParentDID, nil
	}
	parent := path.Dir(p)
	did, err := itx.ensureCleanDir(ctx, parent)
	if err != nil {
		return 0, err
	}
	itx.cacheParent(parent, did)
	return did, nil
}

func (itx *IndexTx) cacheParent(parent string, did int64) {
	itx.cachedParent = parent
	itx.cachedParentDID = did
}

func sameParentPath(p, parent string) bool {
	if parent == "" {
		return false
	}
	if parent == "/" {
		return strings.LastIndexByte(p, '/') == 0
	}
	if len(p) <= len(parent) || p[len(parent)] != '/' || !strings.HasPrefix(p, parent) {
		return false
	}
	return !strings.Contains(p[len(parent)+1:], "/")
}

// Add buffers one node for insertion. It cleans the path, skips the root record
// (root is never its own child), resolves the parent directory ID, ensures a dir
// ID for directory nodes, derives name/name_ci/type, normalizes is_dir, and
// records mtime as Unix seconds + zone offset + a known flag (a zero time.Time
// stores mtime_known=0 and can't round-trip through UnixNano). When the buffer
// reaches batchRows it flushes; the disk ceiling is re-checked on the
// diskCheckRows cadence.
func (itx *IndexTx) Add(ctx context.Context, n model.BrowseNode) error {
	if itx.failed != nil {
		return itx.failed
	}
	p := model.CleanBrowsePath(n.Path)
	if p == "/" {
		return nil
	}
	parentDID, err := itx.parentDID(ctx, p)
	if err != nil {
		itx.failed = err
		return err
	}
	name := model.BrowseName(n.Name, p)
	typ := normalizeType(n.Type, n.IsDir)
	isDir := n.IsDir || typ == "dir"
	if isDir {
		did, err := itx.ensureCleanDir(ctx, p)
		if err != nil {
			itx.failed = err
			return err
		}
		itx.cacheParent(p, did)
	} else {
		itx.subtreeSizes[parentDID-itx.firstDID] += n.Size
	}
	row := nodeRow{
		parentDID:  parentDID,
		name:       name,
		typ:        typ,
		isDir:      isDir,
		size:       n.Size,
		perms:      n.Permissions,
		uid:        n.UID,
		gid:        n.GID,
		ownerKnown: n.OwnerKnown,
		linkTarget: n.LinkTarget,
		nameCI:     strings.ToLower(name),
	}
	if !n.ModTime.IsZero() {
		_, offset := n.ModTime.Zone()
		row.mtimeKnown = true
		row.mtimeUnix = n.ModTime.Unix()
		row.mtimeOffsetSec = offset
	}
	itx.buf = append(itx.buf, row)
	itx.count++

	if len(itx.buf) >= batchRows {
		if err := itx.flush(ctx); err != nil {
			return err
		}
	}
	itx.sinceDiskCheck++
	if itx.sinceDiskCheck >= diskCheckRows {
		itx.sinceDiskCheck = 0
		if err := itx.checkDisk(); err != nil {
			itx.failed = err
			return err
		}
	}
	return nil
}

// Count reports the number of accepted Add calls, for live progress. The final
// committed row count is tracked from successful inserts.
func (itx *IndexTx) Count() int { return itx.count }

// foldSubtreeSizes rolls each directory's accumulated file bytes up into all of
// its ancestors and stamps the total onto every buffered dir row. dirBuf is in
// did-ascending order (ensureCleanDir assigns a child a strictly larger did than
// its parent), so iterating in reverse visits children before parents: adding a
// child's complete subtree to its parent is therefore safe in a single pass. The
// root (parentDID 0, below firstDID) has no parent to add into.
func (itx *IndexTx) foldSubtreeSizes() {
	for i := len(itx.dirBuf) - 1; i >= 0; i-- {
		did := itx.dirBuf[i].did
		if pd := itx.dirBuf[i].parentDID; pd >= itx.firstDID {
			itx.subtreeSizes[pd-itx.firstDID] += itx.subtreeSizes[did-itx.firstDID]
		}
	}
	for i := range itx.dirBuf {
		itx.dirBuf[i].subtreeSize = itx.subtreeSizes[itx.dirBuf[i].did-itx.firstDID]
	}
}

// checkDisk enforces the optional disk ceiling over all regular files in the DB
// directory, returning a wrapped model.ErrBrowseDiskLimit when exceeded.
func (itx *IndexTx) checkDisk() error {
	if itx.db.maxDiskBytes == 0 {
		return nil
	}
	size, err := itx.db.dirSize()
	if err != nil {
		return fmt.Errorf("browsedb disk-check: %w", err)
	}
	if size > itx.db.maxDiskBytes {
		return fmt.Errorf("browsedb index: %w", model.ErrBrowseDiskLimit)
	}
	return nil
}

// Commit is terminal: it flushes the final batch, re-checks the disk ceiling,
// stores the successful insert count, marks the snapshots row indexed in the same
// tx, and commits. Any failure rolls back the underlying tx and returns the
// path-free error without marking the snapshot indexed. The index is maintained
// incrementally during the load (see BeginIndex), so there is no post-load
// rebuild here.
func (itx *IndexTx) Commit(ctx context.Context) error {
	if itx.failed != nil {
		_ = itx.tx.Rollback()
		return itx.failed
	}
	if err := itx.flush(ctx); err != nil {
		_ = itx.tx.Rollback()
		return err
	}
	itx.foldSubtreeSizes()
	if err := itx.flushDirs(ctx); err != nil {
		_ = itx.tx.Rollback()
		return err
	}
	if err := itx.checkDisk(); err != nil {
		itx.failed = err
		_ = itx.tx.Rollback()
		return err
	}
	if _, err := itx.tx.ExecContext(ctx,
		`UPDATE snapshots SET entries=?, indexed_at_unix=? WHERE sid=?`,
		itx.insertedRows, time.Now().Unix(), itx.sid); err != nil {
		itx.failed = fmt.Errorf("browsedb marker: %w", err)
		_ = itx.tx.Rollback()
		return itx.failed
	}
	if err := itx.tx.Commit(); err != nil {
		itx.failed = fmt.Errorf("browsedb commit: %w", err)
		_ = itx.tx.Rollback()
		return itx.failed
	}
	return nil
}

// Rollback discards the transaction. It is idempotent: a tx already finished
// (committed or rolled back) returns nil; any other rollback error is wrapped.
func (itx *IndexTx) Rollback() error {
	err := itx.tx.Rollback()
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return fmt.Errorf("browsedb rollback: %w", err)
}

// normalizeType maps restic's raw node type to a stored type, defaulting empty
// input to "dir" when the node is a directory else "file".
func normalizeType(t string, isDir bool) string {
	if t != "" {
		return t
	}
	if isDir {
		return "dir"
	}
	return "file"
}
