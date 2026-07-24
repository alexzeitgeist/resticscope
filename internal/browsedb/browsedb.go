// Package browsedb provides session-scoped, Adiantum-encrypted SQLite indexes for
// snapshot browsing. Filenames and directory paths are persisted only in those
// indexes. Each database uses a random 32-byte session key that is neither stored
// nor derived from a repository password. Clean shutdown removes the database;
// stale files are unreadable after key loss and scavenged at startup. Errors omit
// node names and paths, and repository keys are names or IDs, not backend targets.
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

// ErrFilesystem is the path-free fallback when an error cannot be reduced to a
// bare *os.PathError.Err.
var ErrFilesystem = errors.New("filesystem error")

// schemaSnapshots maps repositories and snapshots to IDs. indexed_at_unix is
// NULL while an ID is reserved and becomes the IsIndexed gate after commit.
const schemaSnapshots = `CREATE TABLE IF NOT EXISTS snapshots (
  sid INTEGER PRIMARY KEY,
  repo TEXT NOT NULL,
  snapshot TEXT NOT NULL,
  entries INTEGER NOT NULL DEFAULT 0,
  indexed_at_unix INTEGER,
  UNIQUE (repo, snapshot)
)`

// schemaDirs interns paths per snapshot. Globally unique directory IDs support
// parent-only node lookups; Commit computes recursive subtree sizes bottom-up.
const schemaDirs = `CREATE TABLE IF NOT EXISTS dirs (
  did INTEGER PRIMARY KEY,
  sid INTEGER NOT NULL,
  path TEXT NOT NULL,
  subtree_size INTEGER NOT NULL DEFAULT 0,
  UNIQUE (sid, path)
)`

// schemaNodes uses implicit row IDs as index locators. Snapshot and directory
// IDs intern repeated text, and name_ci stores the lowercase BINARY sort key.
const schemaNodes = `CREATE TABLE IF NOT EXISTS nodes (
  sid INTEGER NOT NULL, parent_did INTEGER NOT NULL, name TEXT NOT NULL,
  type TEXT NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL,
  mtime_unix INTEGER NOT NULL, mtime_offset_sec INTEGER NOT NULL, mtime_known INTEGER NOT NULL,
  perms TEXT NOT NULL, uid INTEGER NOT NULL, gid INTEGER NOT NULL,
  owner_known INTEGER NOT NULL, link_target TEXT NOT NULL, name_ci TEXT NOT NULL
)`

// schemaIndex is maintained during bulk loading because a deferred index build
// can exhaust the 256 MiB WASM heap. Precomputed name_ci avoids COLLATE while
// preserving the name tie-break.
const schemaIndex = `CREATE INDEX IF NOT EXISTS idx_nodes_dir ON nodes (parent_did, is_dir DESC, name_ci, name)`

// DB is the session's encrypted store for indexed repositories and snapshots.
type DB struct {
	pool         *sql.DB
	dir          string
	maxDiskBytes int64
}

// sqliteURIPath escapes SQLite file-URI path metacharacters. An unescaped "?" or
// "#" in the operator-supplied cache dir terminates the URI before "?vfs=adiantum",
// so the DB would open on the default plaintext VFS and persist filenames in the
// clear. Escaping "%" preserves literal paths through SQLite's percent-decoding;
// NewReplacer scans the input once and never re-examines its own output, so "%"
// is not double-encoded.
func sqliteURIPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
}

// Open opens or creates the encrypted browse database using a 32-byte key.
// A zero maxDiskBytes is unlimited; a positive value caps regular files in the
// database directory and is checked periodically during indexing. Encryption is
// configured for every connection without placing the key in the URI.
func Open(path string, key []byte, maxDiskBytes int64) (*DB, error) {
	return OpenContext(context.Background(), path, key, maxDiskBytes)
}

// OpenContext opens the encrypted browse DB like Open, using ctx while applying
// the schema.
func OpenContext(ctx context.Context, path string, key []byte, maxDiskBytes int64) (*DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("browsedb open: %w", errInvalidKey)
	}
	if maxDiskBytes < 0 {
		return nil, fmt.Errorf("browsedb open: %w", errInvalidDiskLimit)
	}
	hexkey := hex.EncodeToString(key)
	dsn := "file:" + sqliteURIPath(path) + "?vfs=adiantum"
	pool, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		// Set the key before any page access; the remaining PRAGMAs tune bulk ingest.
		if err := c.Exec("PRAGMA hexkey='" + hexkey + "'"); err != nil {
			return fmt.Errorf("hexkey: %w", err)
		}
		for _, p := range []string{
			// Align SQLite pages with Adiantum's fixed 4096-byte blocks.
			"PRAGMA page_size = 4096",
			// Keep transient B-trees in memory to avoid encrypted temporary-file I/O.
			"PRAGMA temp_store = memory",
			// Keep the encrypted journal on disk; large in-memory journals can exhaust
			// the driver's 256 MiB WASM heap.
			"PRAGMA journal_mode = DELETE",
			"PRAGMA synchronous = OFF",
			// Bound WASM heap use with a 64 MiB cache backed by the encrypted database.
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
	// One connection is sufficient: reads begin only after the sole writer commits.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	db := &DB{pool: pool, dir: filepath.Dir(path), maxDiskBytes: maxDiskBytes}
	if err := db.applySchema(ctx); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) applySchema(ctx context.Context) error {
	// Index last; it is then maintained incrementally, never rebuilt (see schemaIndex).
	for _, stmt := range []string{schemaSnapshots, schemaDirs, schemaNodes, schemaIndex} {
		if _, err := db.pool.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("browsedb schema: %w", err)
		}
	}
	return nil
}

// Close closes the connection pool. The session wrapper owns removal of the
// database directory and lock file.
func (db *DB) Close() error {
	if err := db.pool.Close(); err != nil {
		return fmt.Errorf("browsedb close: %w", err)
	}
	return nil
}

// dirSize conservatively counts every regular file in the database directory.
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

// PathFreeFSError strips paths from filesystem errors. It returns nil for nil,
// the inner error for a non-empty *os.PathError, and ErrFilesystem otherwise.
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
