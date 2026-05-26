// Package browsedb owns the session-scoped, encrypted-at-rest SQLite store that
// backs the in-app snapshot browser. The first time a snapshot is browsed its
// whole namespace is streamed into the DB once; all later directory navigation
// is served by SQL queries.
//
// Privacy contract: filenames are persisted ONLY in this DB, which is encrypted
// at rest by the adiantum VFS under a random 32-byte key that lives only in
// memory (never derived from the repo password, never written anywhere). The DB
// file is created lazily on first browse and removed on clean exit; a crash
// leftover is unreadable because the key is gone, and conservative startup
// cleanup scavenges demonstrably-stale leftovers. To uphold that contract every
// error returned from this package is path-free: it wraps an operation name and
// a sentinel/driver cause, never a node path, name, or directory string (those
// only ever flow through bound query parameters, which SQLite never echoes into
// error messages). The repo key is the validated configured repo name/ID passed
// by the app, never a backend URL/bucket/credential-bearing target.
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
// per column of the nodes table. It is fixed by the schema below.
const insertBindParams = 17

// batchRows is how many node rows accumulate before a single multi-row INSERT is
// flushed inside the index transaction. Derived from the verify-first PoC, which
// reported the pinned driver's LIMIT_VARIABLE_NUMBER = 32766: a batch binds
// batchRows*insertBindParams parameters, so batchRows = min(250, floor(32766/17)
// = 1927) = 250. The 250 ceiling keeps a flush's buffer and parameter slice
// small; the floor guards against a build whose variable limit is unexpectedly
// low.
const batchRows = 250

// diskCheckRows is the coarse cadence (in accepted Add calls) at which the disk
// ceiling is re-measured during indexing. The ceiling is a safety cap with
// bounded overshoot between checks, not a precise quota, so a per-node stat is
// deliberately avoided; Commit always checks once more before marking indexed.
const diskCheckRows = 10000

// rowPlaceholder is the "(?,?,...)" group bound for one node row, built once so
// the bind count cannot drift from insertBindParams.
var rowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", insertBindParams), ",") + ")"

var (
	errInvalidKey       = errors.New("encryption key must be 32 bytes")
	errInvalidDiskLimit = errors.New("max disk bytes must be >= 0")
)

const schemaNodes = `CREATE TABLE IF NOT EXISTS nodes (
  repo TEXT NOT NULL, snapshot TEXT NOT NULL,
  path TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL,
  type TEXT NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL,
  mtime_unix INTEGER NOT NULL, mtime_offset_sec INTEGER NOT NULL, mtime_known INTEGER NOT NULL,
  perms TEXT NOT NULL, uid INTEGER NOT NULL, gid INTEGER NOT NULL,
  owner_known INTEGER NOT NULL, link_target TEXT NOT NULL, name_ci TEXT NOT NULL,
  PRIMARY KEY (repo, snapshot, path)
) WITHOUT ROWID`

const schemaIndex = `CREATE INDEX IF NOT EXISTS idx_nodes_dir ON nodes (repo, snapshot, parent, is_dir DESC, name_ci, name)`

const schemaMarkers = `CREATE TABLE IF NOT EXISTS indexed_snapshots (
  repo TEXT NOT NULL, snapshot TEXT NOT NULL,
  entries INTEGER NOT NULL, indexed_at_unix INTEGER NOT NULL,
  PRIMARY KEY (repo, snapshot)
) WITHOUT ROWID`

// insertColumns lists the nodes columns in bind order; insertSuffix is the
// upsert clause refreshing every non-PK column from the conflicting row.
const insertColumns = `(repo,snapshot,path,parent,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target,name_ci)`

const insertSuffix = ` ON CONFLICT(repo,snapshot,path) DO UPDATE SET ` +
	`parent=excluded.parent,name=excluded.name,type=excluded.type,is_dir=excluded.is_dir,size=excluded.size,` +
	`mtime_unix=excluded.mtime_unix,mtime_offset_sec=excluded.mtime_offset_sec,mtime_known=excluded.mtime_known,` +
	`perms=excluded.perms,uid=excluded.uid,gid=excluded.gid,owner_known=excluded.owner_known,` +
	`link_target=excluded.link_target,name_ci=excluded.name_ci`

const listDirQuery = `SELECT path,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target ` +
	`FROM nodes WHERE repo=? AND snapshot=? AND parent=? ORDER BY is_dir DESC, name_ci, name`

// DB is a handle to the session's encrypted browse store. One DB per app
// session backs every repo/snapshot indexed during that run; rows are keyed by
// (repo, snapshot, path).
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
			"PRAGMA temp_store = memory",
			"PRAGMA journal_mode = DELETE",
			"PRAGMA synchronous = OFF",
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
	for _, stmt := range []string{schemaNodes, schemaIndex, schemaMarkers} {
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
		`SELECT 1 FROM indexed_snapshots WHERE repo=? AND snapshot=?`, repo, snapshot).Scan(&one)
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
	rows, err := db.pool.QueryContext(ctx, listDirQuery, repo, snapshot, parent)
	if err != nil {
		return nil, fmt.Errorf("browsedb list-dir: %w", err)
	}
	defer rows.Close()

	var out []model.BrowseEntry
	for rows.Next() {
		var (
			e          model.BrowseEntry
			isDir      int
			mtimeUnix  int64
			mtimeOff   int
			mtimeKnown int
			ownerKnown int
			uid, gid   int64
		)
		if err := rows.Scan(&e.Path, &e.Name, &e.Type, &isDir, &e.Size,
			&mtimeUnix, &mtimeOff, &mtimeKnown, &e.Permissions, &uid, &gid, &ownerKnown, &e.LinkTarget); err != nil {
			return nil, fmt.Errorf("browsedb list-dir scan: %w", err)
		}
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
	parent         string
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

// IndexTx is a single, terminal index transaction for one (repo, snapshot). Add
// buffers rows and flushes them in bounded multi-row batches; Commit writes the
// indexed_snapshots marker and commits atomically, so a snapshot is marked
// indexed only on a clean, complete pass. Once any Add/flush/check fails the tx
// is poisoned: it holds that path-free error and every later Add/Commit returns
// it, while Rollback stays safe and idempotent.
type IndexTx struct {
	db       *DB
	tx       *sql.Tx
	repo     string
	snapshot string

	buf            []nodeRow
	count          int // accepted Add calls, for progress reporting
	sinceDiskCheck int
	failed         error // sticky, path-free; set on first failure
}

// BeginIndex starts an index transaction for (repo, snapshot). The caller must
// Commit on a complete stream or Rollback on cancel/error/incomplete.
func (db *DB) BeginIndex(ctx context.Context, repo, snapshot string) (*IndexTx, error) {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("browsedb begin-index: %w", err)
	}
	return &IndexTx{db: db, tx: tx, repo: repo, snapshot: snapshot}, nil
}

// Add buffers one node for insertion. It cleans the path, skips the root record
// (root is never its own child), derives name/parent/name_ci/type, normalizes
// is_dir, and records mtime as Unix seconds + zone offset + a known flag (a zero
// time.Time stores mtime_known=0 and can't round-trip through UnixNano). When the
// buffer reaches batchRows it flushes; the disk ceiling is re-checked on the
// diskCheckRows cadence.
func (itx *IndexTx) Add(ctx context.Context, n model.BrowseNode) error {
	if itx.failed != nil {
		return itx.failed
	}
	p := model.CleanBrowsePath(n.Path)
	if p == "/" {
		return nil
	}
	name := model.BrowseName(n.Name, p)
	typ := normalizeType(n.Type, n.IsDir)
	row := nodeRow{
		path:       p,
		parent:     path.Dir(p),
		name:       name,
		typ:        typ,
		isDir:      n.IsDir || typ == "dir",
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
// authoritative unique row count is computed at Commit and stored on the marker.
func (itx *IndexTx) Count() int { return itx.count }

// flush writes the buffered rows as one multi-row upsert. The batch is
// de-duplicated by normalized path (last write wins, first-seen position kept)
// so a within-batch path collision cannot violate SQLite's rule against
// upserting the same row twice in one statement.
func (itx *IndexTx) flush(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	if len(itx.buf) == 0 {
		return nil
	}
	rows := dedupeRows(itx.buf)
	itx.buf = itx.buf[:0]

	query, args := buildInsert(itx.repo, itx.snapshot, rows)
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb index: %w", err)
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
// computes the unique row count, writes the indexed_snapshots marker in the same
// tx, and commits. Any failure rolls back the underlying tx and returns the
// path-free error without marking the snapshot indexed.
func (itx *IndexTx) Commit(ctx context.Context) error {
	if itx.failed != nil {
		_ = itx.tx.Rollback()
		return itx.failed
	}
	if err := itx.flush(ctx); err != nil {
		_ = itx.tx.Rollback()
		return err
	}
	if err := itx.checkDisk(); err != nil {
		itx.failed = err
		_ = itx.tx.Rollback()
		return err
	}
	var entries int64
	if err := itx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE repo=? AND snapshot=?`, itx.repo, itx.snapshot).Scan(&entries); err != nil {
		itx.failed = fmt.Errorf("browsedb count: %w", err)
		_ = itx.tx.Rollback()
		return itx.failed
	}
	// Plain INSERT: the app owns the IsIndexed gate under indexMu, so a marker PK
	// conflict is a caller bug and must surface as a path-free error, not a
	// silent replace.
	if _, err := itx.tx.ExecContext(ctx,
		`INSERT INTO indexed_snapshots (repo,snapshot,entries,indexed_at_unix) VALUES (?,?,?,?)`,
		itx.repo, itx.snapshot, entries, time.Now().Unix()); err != nil {
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

// buildInsert assembles the multi-row upsert and its bound args for rows.
func buildInsert(repo, snapshot string, rows []nodeRow) (string, []any) {
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
			repo, snapshot, r.path, r.parent, r.name, r.typ, boolInt(r.isDir), r.size,
			r.mtimeUnix, r.mtimeOffsetSec, boolInt(r.mtimeKnown), r.perms,
			int64(r.uid), int64(r.gid), boolInt(r.ownerKnown), r.linkTarget, r.nameCI)
	}
	b.WriteString(insertSuffix)
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
