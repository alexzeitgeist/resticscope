// Package browsedb owns the session-scoped, encrypted-at-rest SQLite store that
// backs the in-app snapshot browser. The first time a snapshot is browsed its
// whole namespace is streamed into the DB once; all later directory navigation
// is served by SQL queries.
//
// Privacy contract: filenames and directory paths are persisted ONLY in this DB,
// which is encrypted at rest by the adiantum VFS under a random 32-byte key that
// lives only in memory (never derived from the repo password, never written
// anywhere). The DB file is created lazily on first browse and removed on clean
// exit; a crash leftover is unreadable because the key is gone, and conservative
// startup cleanup scavenges demonstrably-stale leftovers. To uphold that contract
// every error returned from this package is path-free: it wraps an operation name
// and a sentinel/driver cause, never a node path, name, or directory string
// (those only ever flow through bound query parameters, which SQLite never echoes
// into error messages). The repo key is the validated configured repo name/ID
// passed by the app, never a backend URL/bucket/credential-bearing target.
package browsedb

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"resticscope/internal/model"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // registers the "adiantum" encrypting VFS
)

// insertBindParams is the number of bound parameters per inserted node row — one
// per column of the nodes table (sid + 14 node fields). It is fixed by the schema
// below.
const insertBindParams = 15

const dirInsertBindParams = 3

// batchRows is how many node rows accumulate before a single multi-row INSERT is
// flushed inside the index transaction. The pinned driver's
// LIMIT_VARIABLE_NUMBER = 32766 caps a batch at floor(32766/15) = 2184 rows; 1000
// binds 15000 params/batch, ~4× fewer ExecContext round-trips than the old 250
// with comfortable headroom under that cap.
const batchRows = 1000

const dirBatchRows = 1000

// diskCheckRows is the coarse cadence (in accepted Add calls) at which the disk
// ceiling is re-measured during indexing. The ceiling is a safety cap with
// bounded overshoot between checks, not a precise quota, so a per-node stat is
// deliberately avoided; Commit always checks once more before marking indexed.
const diskCheckRows = 10000

// rowPlaceholder is the "(?,?,...)" group bound for one node row, built once so
// the bind count cannot drift from insertBindParams.
var rowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", insertBindParams), ",") + ")"

var dirRowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", dirInsertBindParams), ",") + ")"

var (
	errInvalidKey       = errors.New("encryption key must be 32 bytes")
	errInvalidDiskLimit = errors.New("max disk bytes must be >= 0")
)

// schemaSnapshots folds the dictionary (sid ⇄ repo+snapshot) and the indexed
// marker into one small table, one row per indexed snapshot. indexed_at_unix is
// NULL while a sid is reserved by an in-flight tx (it rolls back with that tx) and
// NOT NULL once committed — the latter is the IsIndexed gate. sid replaces the
// 64-char snapshot hex stored on every node row.
const schemaSnapshots = `CREATE TABLE IF NOT EXISTS snapshots (
  sid INTEGER PRIMARY KEY,
  repo TEXT NOT NULL,
  snapshot TEXT NOT NULL,
  entries INTEGER NOT NULL DEFAULT 0,
  indexed_at_unix INTEGER,
  UNIQUE (repo, snapshot)
)`

// schemaDirs interns directory paths once per snapshot. did is globally unique,
// so nodes can key their browse lookup by parent_did alone.
const schemaDirs = `CREATE TABLE IF NOT EXISTS dirs (
  did INTEGER PRIMARY KEY,
  sid INTEGER NOT NULL,
  path TEXT NOT NULL,
  UNIQUE (sid, path)
)`

// schemaNodes is an implicit-rowid table (no WITHOUT ROWID): sid replaces the
// repo+snapshot TEXT, parent_did replaces repeated parent path text, and the
// index locator is a ~5-byte rowid varint instead of the full composite PK.
// name_ci is the precomputed lowercase fold (see "name_ci" in the plan) sorted
// with native BINARY.
const schemaNodes = `CREATE TABLE IF NOT EXISTS nodes (
  sid INTEGER NOT NULL, parent_did INTEGER NOT NULL, name TEXT NOT NULL,
  type TEXT NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL,
  mtime_unix INTEGER NOT NULL, mtime_offset_sec INTEGER NOT NULL, mtime_known INTEGER NOT NULL,
  perms TEXT NOT NULL, uid INTEGER NOT NULL, gid INTEGER NOT NULL,
  owner_known INTEGER NOT NULL, link_target TEXT NOT NULL, name_ci TEXT NOT NULL
)`

// schemaIndex is created once at Open and maintained incrementally during the
// bulk load (it is NOT dropped/rebuilt per run: a deferred CREATE INDEX would sort
// all rows in the 256 MiB wasm heap and OOM on a multi-million-row snapshot).
// No COLLATE clause: name_ci is precomputed lowercase, sorted BINARY, preserving
// the pinned tie-break (see "name_ci" in the plan).
const schemaIndex = `CREATE INDEX IF NOT EXISTS idx_nodes_dir ON nodes (parent_did, is_dir DESC, name_ci, name)`

// insertColumns lists the nodes columns in bind order, sid first, no path. The
// upsert is gone: restic emits each tree path once, so a plain INSERT is correct
// (within-batch dedupe stays as cheap insurance).
const insertColumns = `(sid,parent_did,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target,name_ci)`

// listDirDIDQuery resolves a requested committed directory path to its did. A
// never-indexed snapshot or never-seen directory yields sql.ErrNoRows and an
// empty listing.
const listDirDIDQuery = `SELECT d.did FROM dirs d JOIN snapshots s ON s.sid=d.sid ` +
	`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND d.path=?`

// listDirQuery uses parent_did to fetch one directory's immediate children.
// Paths are reconstructed in Go from the requested parent path plus each row's
// name.
const listDirQuery = `SELECT name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target ` +
	`FROM nodes WHERE parent_did=? ` +
	`ORDER BY is_dir DESC, name_ci, name`

// errAlreadyIndexed is the path-free sentinel BeginIndex returns when a
// (repo,snapshot) is already committed. The app gates on IsIndexed under indexMu,
// so this is the defensive path that preserves the "no silent replace" contract.
var errAlreadyIndexed = errors.New("snapshot already indexed")

// DB is a handle to the session's encrypted browse store. One DB per app
// session backs every repo/snapshot indexed during that run; each snapshot gets
// an integer sid (see the snapshots table) and node rows reference interned
// directories by did.
type DB struct {
	pool         *sql.DB
	sqlitePath   string
	dir          string
	maxDiskBytes int64
}

// Open opens (creating if absent) the encrypted browse DB at path, keyed by the
// 32-byte key, and applies the schema. maxDiskBytes==0 means unlimited; a
// positive value caps the total size of all regular files in the DB's directory,
// enforced on a coarse cadence during indexing. The key is hex-encoded and set
// via PRAGMA hexkey in a per-connection init callback so every (re)connection is
// keyed without ever placing the key in the DSN/URI.
func Open(path string, key []byte, maxDiskBytes int64) (*DB, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("browsedb open: %w", errInvalidKey)
	}
	if maxDiskBytes < 0 {
		return nil, fmt.Errorf("browsedb open: %w", errInvalidDiskLimit)
	}
	hexkey := hex.EncodeToString(key)
	dsn := "file:" + path + "?vfs=adiantum"
	pool, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		// hexkey must be the first PRAGMA so the key is in place before any page
		// is read or written; the rest tune bulk ingest and keep temp B-trees off
		// any plaintext file.
		if err := c.Exec("PRAGMA hexkey='" + hexkey + "'"); err != nil {
			return fmt.Errorf("hexkey: %w", err)
		}
		for _, p := range []string{
			// adiantum encrypts in fixed 4096-byte blocks keyed on file offset
			// (vfs/adiantum/hbsh.go:72), so one page == one block — the most aligned
			// choice and zero crypto saving from a larger page. Set explicitly as a
			// guard.
			"PRAGMA page_size = 4096",
			// temp_store=memory keeps any transient B-tree off a temp file. Index
			// maintenance is incremental (see schemaIndex), so no large sorter runs
			// here — the heavy spill goes to the encrypted main DB via the page cache.
			"PRAGMA temp_store = memory",
			// journal_mode=DELETE keeps the rollback journal on disk (encrypted by the
			// adiantum VFS), NOT in the wasm heap. The driver's SQLite runs in a wasm
			// module capped at 256 MiB (ncruces sqlite3_wrap.Memory{Max:4096}); a
			// MEMORY journal for a multi-million-row index tx would exhaust that heap
			// and the wrapper panics on the failed alloc (alloc.go OOMErr), so the
			// journal must stay off-heap.
			"PRAGMA journal_mode = DELETE",
			"PRAGMA synchronous = OFF",
			// 64 MiB cache: dirty pages spill to the encrypted main DB file as the
			// cache fills, bounding wasm-heap use during a huge bulk load.
			"PRAGMA cache_size = -65536",
		} {
			if err := c.Exec(p); err != nil {
				return fmt.Errorf("pragma: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("browsedb open: %w", err)
	}
	// One connection serializes all access: there is a single writer (the index
	// tx) and reads happen only after it commits, so no concurrency is needed.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	db := &DB{pool: pool, sqlitePath: path, dir: filepath.Dir(path), maxDiskBytes: maxDiskBytes}
	if err := db.applySchema(context.Background()); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) applySchema(ctx context.Context) error {
	// Index last (IF NOT EXISTS): created once here and then maintained
	// incrementally as rows are inserted (never dropped/rebuilt — see schemaIndex).
	for _, stmt := range []string{schemaSnapshots, schemaDirs, schemaNodes, schemaIndex} {
		if _, err := db.pool.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("browsedb schema: %w", err)
		}
	}
	return nil
}

// IsIndexed reports whether (repo, snapshot) has a committed index marker.
func (db *DB) IsIndexed(ctx context.Context, repo, snapshot string) (bool, error) {
	var one int
	err := db.pool.QueryRowContext(ctx,
		`SELECT 1 FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`, repo, snapshot).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("browsedb is-indexed: %w", err)
	}
	return true, nil
}

// ListDir returns the immediate children of dir within (repo, snapshot),
// directories first then case-insensitively by name with deterministic
// tie-breakers. A never-indexed (repo, snapshot) yields an empty slice and nil
// error; callers that must distinguish "indexed but empty" use IsIndexed.
func (db *DB) ListDir(ctx context.Context, repo, snapshot, dir string) ([]model.BrowseEntry, error) {
	parent := model.CleanBrowsePath(dir)
	var did int64
	err := db.pool.QueryRowContext(ctx, listDirDIDQuery, repo, snapshot, parent).Scan(&did)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("browsedb list-dir: %w", err)
	}

	rows, err := db.pool.QueryContext(ctx, listDirQuery, did)
	if err != nil {
		return nil, fmt.Errorf("browsedb list-dir: %w", err)
	}
	defer rows.Close()

	var out []model.BrowseEntry
	for rows.Next() {
		var (
			e          model.BrowseEntry
			name       string
			isDir      int
			mtimeUnix  int64
			mtimeOff   int
			mtimeKnown int
			ownerKnown int
			uid, gid   int64
		)
		if err := rows.Scan(&name, &e.Type, &isDir, &e.Size,
			&mtimeUnix, &mtimeOff, &mtimeKnown, &e.Permissions, &uid, &gid, &ownerKnown, &e.LinkTarget); err != nil {
			return nil, fmt.Errorf("browsedb list-dir scan: %w", err)
		}
		e.Name = name
		// path is not stored; derive it from the requested parent and row name.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid)
		if mtimeKnown != 0 {
			// FixedZone preserves the wall-clock minute the old browse table
			// displayed; Location().String() is intentionally not meaningful.
			e.ModTime = time.Unix(mtimeUnix, 0).In(time.FixedZone("", mtimeOff))
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("browsedb list-dir rows: %w", err)
	}
	return out, nil
}

// Close closes the connection pool and removes db.sqlite plus any SQLite sidecar
// files for that basename (rollback-journal/temp leftovers). The cmd-level
// session wrapper removes the whole session directory on clean shutdown.
func (db *DB) Close() error {
	err := db.pool.Close()
	_ = os.Remove(db.sqlitePath)
	if matches, gerr := filepath.Glob(db.sqlitePath + "-*"); gerr == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	if err != nil {
		return fmt.Errorf("browsedb close: %w", err)
	}
	return nil
}

// ClosePreserveFiles closes the connection pool without deleting db.sqlite or
// sidecars. It exists for the internal benchmark command, which needs to inspect
// DB files after a run; production callers should keep using Close.
func (db *DB) ClosePreserveFiles() error {
	if err := db.pool.Close(); err != nil {
		return fmt.Errorf("browsedb close: %w", err)
	}
	return nil
}

// EntryCount returns the committed entry count recorded for a snapshot. A
// never-indexed snapshot reports 0, nil to match ListDir's empty result contract.
func (db *DB) EntryCount(ctx context.Context, repo, snapshot string) (int64, error) {
	var entries int64
	err := db.pool.QueryRowContext(ctx,
		`SELECT entries FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`,
		repo, snapshot).Scan(&entries)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("browsedb entry-count: %w", err)
	}
	return entries, nil
}

// dirSize totals the size of every regular file in the DB's directory. This
// intentionally counts db.sqlite, rollback journals, SQLite temp files, and the
// negligible session lock file, giving a conservative over-count for the ceiling.
func (db *DB) dirSize() (int64, error) {
	entries, err := os.ReadDir(db.dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // a sidecar may have vanished between ReadDir and Info; skip it
		}
		total += info.Size()
	}
	return total, nil
}

// nodeRow is one buffered row awaiting a batched flush.
type nodeRow struct {
	path           string
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

type dirRow struct {
	did  int64
	path string
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

	buf            []nodeRow
	dirBuf         []dirRow
	count          int // accepted Add calls, for progress reporting
	insertedRows   int64
	nextDID        int64
	sinceDiskCheck int
	dirs           map[string]int64
	failed         error // sticky, path-free; set on first failure
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
	itx.dirs = make(map[string]int64, 1024)
	if _, err := itx.ensureCleanDir(ctx, "/"); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := itx.flushDirs(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
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
	if p != "/" {
		if _, err := itx.ensureCleanDir(ctx, path.Dir(p)); err != nil {
			return 0, err
		}
	}
	did := itx.nextDID
	itx.nextDID++
	itx.dirs[p] = did
	itx.dirBuf = append(itx.dirBuf, dirRow{did: did, path: p})
	if len(itx.dirBuf) >= dirBatchRows {
		if err := itx.flushDirs(ctx); err != nil {
			return 0, err
		}
	}
	return did, nil
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
	parentDID, err := itx.ensureCleanDir(ctx, path.Dir(p))
	if err != nil {
		itx.failed = err
		return err
	}
	name := model.BrowseName(n.Name, p)
	typ := normalizeType(n.Type, n.IsDir)
	isDir := n.IsDir || typ == "dir"
	if isDir {
		if _, err := itx.ensureCleanDir(ctx, p); err != nil {
			itx.failed = err
			return err
		}
	}
	row := nodeRow{
		path:       p,
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
// committed row count is tracked from successful deduped inserts.
func (itx *IndexTx) Count() int { return itx.count }

// flush writes the buffered rows as one multi-row plain INSERT. The batch is
// de-duplicated by normalized path (last write wins, first-seen position kept) as
// cheap within-batch insurance; restic emits each tree path once, so cross-batch
// duplicates are out of contract (there is no unique constraint to upsert against).
func (itx *IndexTx) flush(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	if len(itx.buf) == 0 {
		return nil
	}
	rows := dedupeRows(itx.buf)
	itx.buf = itx.buf[:0]

	query, args := buildInsert(itx.sid, rows)
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb index: %w", err)
		return itx.failed
	}
	itx.insertedRows += int64(len(rows))
	return nil
}

func (itx *IndexTx) flushDirs(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	if len(itx.dirBuf) == 0 {
		return nil
	}
	rows := itx.dirBuf
	itx.dirBuf = nil

	query, args := buildDirInsert(itx.sid, rows)
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb dir: %w", err)
		return itx.failed
	}
	return nil
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
	if err := itx.flushDirs(ctx); err != nil {
		_ = itx.tx.Rollback()
		return err
	}
	if err := itx.checkDisk(); err != nil {
		itx.failed = err
		_ = itx.tx.Rollback()
		return err
	}
	// Mark the reserved row indexed. The sid was reserved in this tx, so this is an
	// UPDATE of our own row — no PK conflict path; IsIndexed/ListDir gate on the now
	// non-NULL indexed_at_unix.
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

// dedupeRows collapses duplicate normalized paths within one batch, keeping the
// last write's values at the first-seen position.
func dedupeRows(buf []nodeRow) []nodeRow {
	seen := make(map[string]int, len(buf))
	out := make([]nodeRow, 0, len(buf))
	for _, r := range buf {
		if idx, ok := seen[r.path]; ok {
			out[idx] = r
			continue
		}
		seen[r.path] = len(out)
		out = append(out, r)
	}
	return out
}

// buildInsert assembles the multi-row plain INSERT and its bound args for rows,
// binding sid plus parent_did and the 13 remaining node fields (the stored path
// is gone — derived on read).
func buildInsert(sid int64, rows []nodeRow) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT INTO nodes ")
	b.WriteString(insertColumns)
	b.WriteString(" VALUES ")
	args := make([]any, 0, len(rows)*insertBindParams)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(rowPlaceholder)
		args = append(args,
			sid, r.parentDID, r.name, r.typ, boolInt(r.isDir), r.size,
			r.mtimeUnix, r.mtimeOffsetSec, boolInt(r.mtimeKnown), r.perms,
			int64(r.uid), int64(r.gid), boolInt(r.ownerKnown), r.linkTarget, r.nameCI)
	}
	return b.String(), args
}

func buildDirInsert(sid int64, rows []dirRow) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT INTO dirs (did,sid,path) VALUES ")
	args := make([]any, 0, len(rows)*dirInsertBindParams)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(dirRowPlaceholder)
		args = append(args, r.did, sid, r.path)
	}
	return b.String(), args
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
