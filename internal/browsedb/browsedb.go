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
	"path/filepath"
	"strings"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // registers the "adiantum" encrypting VFS
)

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
