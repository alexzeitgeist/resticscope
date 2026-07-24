package browsedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// diskCheckRows bounds disk-limit overshoot between periodic Add checks.
// Commit checks again before marking the snapshot indexed.
const diskCheckRows = 10000

// errAlreadyIndexed prevents BeginIndex from replacing a committed snapshot.
// It contains no repository or snapshot identifiers.
var errAlreadyIndexed = errors.New("snapshot already indexed")

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

// dirRow retains parentDID for Commit's bottom-up size fold; only subtreeSize is
// persisted.
type dirRow struct {
	did         int64
	parentDID   int64
	path        string
	subtreeSize int64
}

// IndexTx is a terminal index transaction for one repository snapshot. Commit
// marks the snapshot indexed atomically after all buffered rows are written.
// The first failure is retained and returned by later Add and Commit calls;
// Rollback remains idempotent.
type IndexTx struct {
	db       *DB
	tx       *sql.Tx
	repo     string
	snapshot string
	sid      int64 // allocated by BeginIndex; never exposed through the app API

	buf             []nodeRow
	dirBuf          []dirRow
	insertStmt      *sql.Stmt
	dirInsertStmt   *sql.Stmt
	count           int // accepted Add calls
	insertedRows    int64
	nextDID         int64
	firstDID        int64   // root ID; this transaction's IDs are contiguous from here
	subtreeSizes    []int64 // recursive file bytes indexed by did-firstDID
	sinceDiskCheck  int
	dirs            map[string]int64
	cachedParent    string
	cachedParentDID int64
	failed          error // first path-free failure
}

// BeginIndex starts an index transaction for a repository snapshot. The caller
// must Commit a complete stream or Rollback after cancellation, failure, or an
// incomplete stream. Rollback also discards the transaction's reserved ID.
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
	// Retain directories until Commit can fold their subtree sizes.
	return itx, nil
}

// allocSID reuses an uncommitted reservation or inserts a new snapshot ID.
// Committed reservations return errAlreadyIndexed.
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

// initNextDID starts allocation after the largest persisted directory ID.
func (itx *IndexTx) initNextDID(ctx context.Context) error {
	var maxDID int64
	if err := itx.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(did), 0) FROM dirs`).Scan(&maxDID); err != nil {
		return fmt.Errorf("browsedb dir: %w", err)
	}
	itx.nextDID = maxDID + 1
	return nil
}

// ensureCleanDir interns a cleaned directory path for this snapshot. Assigning
// IDs in Go avoids a database round trip per unique directory.
func (itx *IndexTx) ensureCleanDir(ctx context.Context, p string) (int64, error) {
	if did, ok := itx.dirs[p]; ok {
		return did, nil
	}
	// Parent ID zero terminates Commit's bottom-up fold at the root.
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
	// Directory IDs stay aligned with accumulator offsets until Commit.
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

// sameParentPath reports whether p is an immediate child of parent.
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

// Add buffers a node for insertion, skipping the root record. It preserves an
// unknown modification time separately from the Unix timestamp and checks the
// disk ceiling periodically.
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
	isDir := n.IsDir || typ == model.NodeTypeDir
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

// Count reports the number of accepted Add calls, for live progress.
func (itx *IndexTx) Count() int { return itx.count }

// foldSubtreeSizes writes recursive file totals into buffered directories.
// Descending IDs visit every child before its parent; parent ID zero terminates
// the fold at the root.
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

// Commit is terminal: it flushes the final batch, folds subtree sizes, re-checks
// the disk ceiling, marks the snapshots row indexed in the same tx, and commits.
// Any failure rolls back and returns the path-free error without marking indexed.
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
		return model.NodeTypeDir
	}
	return model.NodeTypeFile
}
