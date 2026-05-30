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
	"sort"
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

const dirInsertBindParams = 4

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
	errReadDBDir        = errors.New("read browse database directory")
)

// ErrFilesystem is the path-free sentinel PathFreeFSError falls back to when a
// filesystem error cannot be reduced to a bare *os.PathError.Err. Callers in any
// layer can detect a browse filesystem failure without ever touching a string
// that might contain a filename (privacy invariant: browse errors are path-free).
var ErrFilesystem = errors.New("filesystem error")

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
// so nodes can key their browse lookup by parent_did alone. subtree_size carries
// the recursive total of every file byte beneath the directory (folded bottom-up
// at Commit); 0 for an empty directory.
const schemaDirs = `CREATE TABLE IF NOT EXISTS dirs (
  did INTEGER PRIMARY KEY,
  sid INTEGER NOT NULL,
  path TEXT NOT NULL,
  subtree_size INTEGER NOT NULL DEFAULT 0,
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
// upsert is gone: restic emits each tree path once, so a plain INSERT is correct.
const insertColumns = `(sid,parent_did,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target,name_ci)`

var fullInsertSQL = buildInsertSQL(batchRows)

var fullDirInsertSQL = buildDirInsertSQL(dirBatchRows)

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

// searchQuery is the global filename search prefilter: a subsequence LIKE over
// the precomputed name_ci column across one committed snapshot, joined to dirs so
// each matched node carries its own parent path for full-path reconstruction. It
// deliberately has NO LIMIT and NO ORDER BY — the LIKE is only a coarse prefilter
// and the result cap is applied after fuzzy scoring (see Search), so the returned
// rows are the true top matches rather than an arbitrary prefix of the scan order.
// ESCAPE '\' makes the LIKE specials escaped by likeSubsequence match literally.
const searchQuery = `SELECT d.path,n.name,n.type,n.is_dir,n.size,n.mtime_unix,n.mtime_offset_sec,n.mtime_known,n.perms,n.uid,n.gid,n.owner_known,n.link_target ` +
	`FROM nodes n ` +
	`JOIN snapshots s ON s.sid=n.sid ` +
	`JOIN dirs d ON d.did=n.parent_did ` +
	`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND n.name_ci LIKE ? ESCAPE '\'`

// defaultSearchLimit caps how many ranked rows a search returns when the caller
// passes a non-positive limit. It bounds the result slice (and the UI state built
// from it) even when a broad query matches a large fraction of the snapshot; Total
// still reports every match so the UI can show "showing N of Total".
const defaultSearchLimit = 200

// errAlreadyIndexed is the path-free sentinel BeginIndex returns when a
// (repo,snapshot) is already committed. The app gates on IsIndexed under its
// session operation lock, so this is the defensive path that preserves the "no
// silent replace" contract.
var errAlreadyIndexed = errors.New("snapshot already indexed")

// DB is a handle to the session's encrypted browse store. One DB per app
// session backs every repo/snapshot indexed during that run; each snapshot gets
// an integer sid (see the snapshots table) and node rows reference interned
// directories by did.
type DB struct {
	pool         *sql.DB
	dir          string
	maxDiskBytes int64
}

// sqliteURIPath percent-encodes the three characters SQLite treats specially
// while parsing a "file:" URI: '%' (its percent-decode marker), '?' (the query
// separator) and '#' (the fragment separator). The DB path is rooted at the
// operator-supplied [global].cache_dir; without this, a directory name
// containing any of these would terminate the URI before "?vfs=adiantum",
// silently dropping the encrypting VFS so the DB would open on the default
// PLAINTEXT VFS — persisting filenames in the clear, a privacy-contract breach.
// SQLite percent-decodes the path, so each escaped char round-trips to the real
// filename. strings.NewReplacer scans the input once and never re-examines its
// own output, so '%' (listed first only for readability) is not double-encoded.
func sqliteURIPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
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
	dsn := "file:" + sqliteURIPath(path) + "?vfs=adiantum"
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

	db := &DB{pool: pool, dir: filepath.Dir(path), maxDiskBytes: maxDiskBytes}
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
	var dirPaths []string
	var zones map[int]*time.Location
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
		if e.IsDir {
			// A directory's listed size is its recursive subtree total, filled in
			// below; clear the raw nodes.size inode value so a zero-subtree dir never
			// leaks it.
			e.Size = 0
			dirPaths = append(dirPaths, e.Path)
		}
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid)
		if mtimeKnown != 0 {
			// FixedZone preserves the wall-clock minute the old browse table
			// displayed; Location().String() is intentionally not meaningful.
			loc := zones[mtimeOff]
			if loc == nil {
				if zones == nil {
					zones = make(map[int]*time.Location, 2)
				}
				loc = time.FixedZone("", mtimeOff)
				zones[mtimeOff] = loc
			}
			e.ModTime = time.Unix(mtimeUnix, 0).In(loc)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("browsedb list-dir rows: %w", err)
	}
	sizes, err := db.dirSizes(ctx, repo, snapshot, dirPaths)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].IsDir {
			if sz, ok := sizes[out[i].Path]; ok {
				out[i].Size = sz
			}
		}
	}
	return out, nil
}

// Search finds nodes anywhere in (repo, snapshot) whose name is a case-folded
// fuzzy (subsequence) match for query, ranked best-first and capped to limit (the
// default cap when limit <= 0). It returns the ranked rows plus Total, the count
// of every match before the cap, so the caller can report a truncated result.
//
// A blank or whitespace-only query returns a zero result with no scan: there is
// nothing to rank. Otherwise the query is lowercased to build a subsequence LIKE
// pattern over name_ci (byte-identical to how name_ci was written, so the
// prefilter never rejects a true match), then every prefiltered row is re-scored
// with model.FuzzyScore on its original name and kept in a bounded top-N using the
// shared model.BetterFuzzy ordering — so DB search and the pure-model ranking
// cannot drift. The original query (not the lowercased pattern) is scored so the
// exact-case bonus still applies. Errors are path-free: they wrap the operation
// name and the driver/scan cause, never a node name or path (those flow only
// through bound parameters, which SQLite never echoes into error text).
func (db *DB) Search(ctx context.Context, repo, snapshot, query string, limit int) (model.BrowseSearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return model.BrowseSearchResult{}, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	pattern := likeSubsequence(strings.ToLower(query))

	rows, err := db.pool.QueryContext(ctx, searchQuery, repo, snapshot, pattern)
	if err != nil {
		return model.BrowseSearchResult{}, fmt.Errorf("browsedb search: %w", err)
	}
	defer rows.Close()

	var (
		top   []model.FuzzyRank // best-first, length bounded by limit
		total int
		zones map[int]*time.Location
	)
	for rows.Next() {
		var (
			e          model.BrowseEntry
			parent     string
			name       string
			isDir      int
			mtimeUnix  int64
			mtimeOff   int
			mtimeKnown int
			ownerKnown int
			uid, gid   int64
		)
		if err := rows.Scan(&parent, &name, &e.Type, &isDir, &e.Size,
			&mtimeUnix, &mtimeOff, &mtimeKnown, &e.Permissions, &uid, &gid, &ownerKnown, &e.LinkTarget); err != nil {
			return model.BrowseSearchResult{}, fmt.Errorf("browsedb search scan: %w", err)
		}
		match, ok := model.FuzzyScore(name, query)
		if !ok {
			// The LIKE prefilter and the subsequence test are equivalent conditions,
			// so this is defensive; skip anything the scorer rejects.
			continue
		}
		total++
		e.Name = name
		// path is not stored on the node; each row carries its own parent.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid)
		if mtimeKnown != 0 {
			loc := zones[mtimeOff]
			if loc == nil {
				if zones == nil {
					zones = make(map[int]*time.Location, 2)
				}
				loc = time.FixedZone("", mtimeOff)
				zones[mtimeOff] = loc
			}
			e.ModTime = time.Unix(mtimeUnix, 0).In(loc)
		}
		top = insertTopN(top, model.FuzzyRank{Entry: e, Match: match}, limit)
	}
	if err := rows.Err(); err != nil {
		return model.BrowseSearchResult{}, fmt.Errorf("browsedb search rows: %w", err)
	}

	out := make([]model.BrowseEntry, len(top))
	var dirPaths []string
	for i, r := range top {
		out[i] = r.Entry
		if out[i].IsDir {
			// Mirror ListDir: a matched directory reports its recursive subtree size,
			// not the raw nodes.size inode value. Zero first, fill from dirSizes below.
			out[i].Size = 0
			dirPaths = append(dirPaths, out[i].Path)
		}
	}
	sizes, err := db.dirSizes(ctx, repo, snapshot, dirPaths)
	if err != nil {
		return model.BrowseSearchResult{}, err
	}
	for i := range out {
		if out[i].IsDir {
			if sz, ok := sizes[out[i].Path]; ok {
				out[i].Size = sz
			}
		}
	}
	return model.BrowseSearchResult{Rows: out, Total: total}, nil
}

// dirSizePathChunk caps how many directory paths are bound into one dirSizes
// query. At one param per path plus the two fixed repo/snapshot binds, this stays
// far under the driver's LIMIT_VARIABLE_NUMBER even though callers (ListDir on a
// huge directory) may ask about thousands of children at once.
const dirSizePathChunk = 900

// dirSizes returns the recursive subtree size for each of paths within
// (repo, snapshot), keyed on the exact dirs.path the writer stored (the same
// model.JoinBrowsePath output ListDir/Search build), so no SQL path
// reconstruction is needed. Only directories with a positive subtree size appear
// in the map; an empty directory (or any path not present) is simply absent, and
// the caller leaves its size at the explicit zero it set. The path list is
// chunked to stay under the bind cap. A never-indexed snapshot yields an empty
// map.
func (db *DB) dirSizes(ctx context.Context, repo, snapshot string, paths []string) (map[string]int64, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make(map[string]int64, len(paths))
	for start := 0; start < len(paths); start += dirSizePathChunk {
		end := start + dirSizePathChunk
		if end > len(paths) {
			end = len(paths)
		}
		chunk := paths[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, repo, snapshot)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := db.pool.QueryContext(ctx, buildDirSizesSQL(len(chunk)), args...)
		if err != nil {
			return nil, fmt.Errorf("browsedb dir-sizes: %w", err)
		}
		for rows.Next() {
			var (
				p    string
				size int64
			)
			if err := rows.Scan(&p, &size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("browsedb dir-sizes scan: %w", err)
			}
			out[p] = size
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("browsedb dir-sizes rows: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// buildDirSizesSQL builds the dirSizes query for a chunk of n directory paths,
// with n bound placeholders in the IN clause after the fixed repo/snapshot binds.
func buildDirSizesSQL(n int) string {
	var b strings.Builder
	b.WriteString(`SELECT d.path,d.subtree_size FROM dirs d JOIN snapshots s ON s.sid=d.sid ` +
		`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND d.subtree_size>0 AND d.path IN (`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	b.WriteByte(')')
	return b.String()
}

// likeSubsequence builds a LIKE pattern matching q as a subsequence: each rune of
// q wrapped in '%' wildcards, so "abc" → "%a%b%c%". The LIKE specials (% _ \) are
// prefixed with a backslash so they match literally under ESCAPE '\'. q is the
// already-lowercased query, matching the BINARY-sorted name_ci column.
func likeSubsequence(q string) string {
	var b strings.Builder
	b.Grow(len(q)*2 + 1)
	b.WriteByte('%')
	for _, r := range q {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		b.WriteByte('%')
	}
	return b.String()
}

// insertTopN keeps top as a best-first slice of at most limit ranks, using the
// shared model.BetterFuzzy ordering. The slice stays sorted, so once it is full a
// candidate that cannot beat the current worst (the last element) is dropped in
// O(1); otherwise it is inserted at its ordered position and the worst is evicted.
// This bounds memory to limit even when a broad query matches a huge fraction of
// the snapshot, while still yielding the exact global top-N.
func insertTopN(top []model.FuzzyRank, r model.FuzzyRank, limit int) []model.FuzzyRank {
	if len(top) >= limit && !model.BetterFuzzy(r, top[len(top)-1]) {
		return top
	}
	// First position r outranks: the predicate is false…false,true…true because top
	// is sorted best-first, so sort.Search finds the correct insertion index.
	i := sort.Search(len(top), func(i int) bool { return model.BetterFuzzy(r, top[i]) })
	if len(top) < limit {
		top = append(top, model.FuzzyRank{})
		copy(top[i+1:], top[i:])
		top[i] = r
		return top
	}
	// Full: shift [i, len-1) down by one (dropping the old worst), then place r.
	copy(top[i+1:], top[i:len(top)-1])
	top[i] = r
	return top
}

// Close closes the connection pool. It deliberately does NOT remove db.sqlite or
// its sidecars: the cmd-level session wrapper owns teardown and removes the whole
// session directory with a single authoritative os.RemoveAll on shutdown, which
// is strictly more thorough (it also takes the directory and the lock file) than
// per-file removal here would be.
func (db *DB) Close() error {
	if err := db.pool.Close(); err != nil {
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
		return 0, fmt.Errorf("%w: %w", errReadDBDir, PathFreeFSError(err))
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

// PathFreeFSError reduces a filesystem error to a path-free form so a browse
// failure never surfaces a filename. An *os.PathError is replaced by its bare
// inner Err (a syscall errno such as "permission denied", which carries no path);
// anything else — including a PathError whose inner Err is nil — collapses to
// ErrFilesystem. A nil error returns nil. This is the single source of truth
// shared by browsedb and the cmd-level session wrapper, both of which must honor
// the path-free invariant.
func PathFreeFSError(err error) error {
	if err == nil {
		return nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil {
		return pathErr.Err
	}
	return ErrFilesystem
}

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
	// Capture the first did before any dir is reserved: within this tx all dids are
	// contiguous from firstDID, so subtreeSizes can be indexed by did-firstDID. The
	// root is the first dir created below, so firstDID == rootDID.
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
		// Accumulate file (and symlink/special) bytes into the immediate parent's
		// subtree total; dir nodes are skipped so a directory's size is "bytes of
		// files beneath it", not its own ~0 inode size. The fold at Commit rolls
		// these into every ancestor. parentDID is a dir resolved in this tx, so
		// parentDID-firstDID is always a valid accumulator index.
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

// flush writes the buffered rows as one multi-row plain INSERT. Full batches
// reuse a tx-scoped prepared statement; the final partial batch keeps its
// dynamically-sized SQL. restic emits each tree path once, so duplicate-path
// collapse is deliberately out of the hot path (there is no unique constraint to
// upsert against).
func (itx *IndexTx) flush(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	if len(itx.buf) == 0 {
		return nil
	}
	rows := itx.buf
	itx.buf = itx.buf[:0]

	args := buildInsertArgs(itx.sid, rows)
	if len(rows) == batchRows {
		stmt, err := itx.fullInsertStmt(ctx)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			itx.failed = fmt.Errorf("browsedb index: %w", err)
			return itx.failed
		}
		itx.insertedRows += int64(len(rows))
		return nil
	}
	query := buildInsertSQL(len(rows))
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb index: %w", err)
		return itx.failed
	}
	itx.insertedRows += int64(len(rows))
	return nil
}

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

// flushDirs writes every buffered dir row in chunks of at most dirBatchRows.
// Because all dir rows are held until Commit (so their subtree sizes can be
// folded), the buffer can far exceed one INSERT's parameter budget; chunking
// keeps each statement at most dirBatchRows*dirInsertBindParams params, well
// under the driver's LIMIT_VARIABLE_NUMBER.
func (itx *IndexTx) flushDirs(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	for len(itx.dirBuf) > 0 {
		n := len(itx.dirBuf)
		if n > dirBatchRows {
			n = dirBatchRows
		}
		chunk := itx.dirBuf[:n]
		if err := itx.flushDirRows(ctx, chunk); err != nil {
			return err
		}
		itx.dirBuf = itx.dirBuf[n:]
	}
	itx.dirBuf = nil
	return nil
}

// flushDirRows writes exactly one chunk of dir rows. A full dirBatchRows chunk
// reuses the tx-scoped prepared statement; a shorter final chunk builds a partial
// INSERT sized to its row count.
func (itx *IndexTx) flushDirRows(ctx context.Context, rows []dirRow) error {
	args := buildDirInsertArgs(itx.sid, rows)
	if len(rows) == dirBatchRows {
		stmt, err := itx.fullDirInsertStmt(ctx)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			itx.failed = fmt.Errorf("browsedb dir: %w", err)
			return itx.failed
		}
		return nil
	}
	query := buildDirInsertSQL(len(rows))
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb dir: %w", err)
		return itx.failed
	}
	return nil
}

func (itx *IndexTx) fullInsertStmt(ctx context.Context) (*sql.Stmt, error) {
	if itx.insertStmt != nil {
		return itx.insertStmt, nil
	}
	stmt, err := itx.tx.PrepareContext(ctx, fullInsertSQL)
	if err != nil {
		itx.failed = fmt.Errorf("browsedb prepare index: %w", err)
		return nil, itx.failed
	}
	itx.insertStmt = stmt
	return stmt, nil
}

func (itx *IndexTx) fullDirInsertStmt(ctx context.Context) (*sql.Stmt, error) {
	if itx.dirInsertStmt != nil {
		return itx.dirInsertStmt, nil
	}
	stmt, err := itx.tx.PrepareContext(ctx, fullDirInsertSQL)
	if err != nil {
		itx.failed = fmt.Errorf("browsedb prepare dir: %w", err)
		return nil, itx.failed
	}
	itx.dirInsertStmt = stmt
	return stmt, nil
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

func buildInsertSQL(n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO nodes ")
	b.WriteString(insertColumns)
	b.WriteString(" VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(rowPlaceholder)
	}
	return b.String()
}

func buildInsertArgs(sid int64, rows []nodeRow) []any {
	args := make([]any, 0, len(rows)*insertBindParams)
	for _, r := range rows {
		args = append(args,
			sid, r.parentDID, r.name, r.typ, boolInt(r.isDir), r.size,
			r.mtimeUnix, r.mtimeOffsetSec, boolInt(r.mtimeKnown), r.perms,
			int64(r.uid), int64(r.gid), boolInt(r.ownerKnown), r.linkTarget, r.nameCI)
	}
	return args
}

func buildDirInsertSQL(n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO dirs (did,sid,path,subtree_size) VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(dirRowPlaceholder)
	}
	return b.String()
}

func buildDirInsertArgs(sid int64, rows []dirRow) []any {
	args := make([]any, 0, len(rows)*dirInsertBindParams)
	for _, r := range rows {
		args = append(args, r.did, sid, r.path, r.subtreeSize)
	}
	return args
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
