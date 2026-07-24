package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/browsedb"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// browseStore adapts *browsedb.DB to app.BrowseStore. It bridges BeginIndex's
// concrete *browsedb.IndexTx to the app.IndexWriter seam and, on Close, tears down
// the whole session — pool, advisory lock, and the encrypted session directory —
// so a clean exit leaves nothing on disk.
type browseStore struct {
	db   *browsedb.DB
	lock *browsedb.SessionLock
	dir  string
}

func (s *browseStore) IsIndexed(ctx context.Context, repo, snapshot string) (bool, error) {
	return s.db.IsIndexed(ctx, repo, snapshot)
}

func (s *browseStore) BeginIndex(ctx context.Context, repo, snapshot string) (app.IndexWriter, error) {
	tx, err := s.db.BeginIndex(ctx, repo, snapshot)
	if err != nil {
		return nil, err // a clean nil interface, never a typed-nil *IndexTx
	}
	return tx, nil
}

func (s *browseStore) ListDir(ctx context.Context, repo, snapshot, dir string) ([]model.BrowseEntry, error) {
	return s.db.ListDir(ctx, repo, snapshot, dir)
}

func (s *browseStore) SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error) {
	return s.db.SubtreeCounts(ctx, repo, snapshot, dir)
}

func (s *browseStore) Search(ctx context.Context, repo, snapshot, query string, limit int) (model.BrowseSearchResult, error) {
	return s.db.Search(ctx, repo, snapshot, query, limit)
}

// Close closes the DB pool, releases the session lock, and removes the entire
// session directory. This wrapper owns whole-directory cleanup, so db.Close only
// closes the pool; RemoveAll is authoritative for whether encrypted browse files
// actually survived. The RemoveAll error is reduced to a path-free form via the
// shared browsedb.PathFreeFSError so a failure never surfaces the session path.
func (s *browseStore) Close() error {
	dbErr := s.db.Close()
	lockErr := s.lock.Close()
	rmErr := browsedb.PathFreeFSError(os.RemoveAll(s.dir))
	return errors.Join(dbErr, lockErr, rmErr)
}

// newBrowseOpen returns the lazy-open closure for the session-scoped encrypted
// browse store. It runs at most once per app run, on the first browse: it mints a
// random 32-byte in-memory key, creates a fresh 0700 session directory under the
// cache dir keyed by a random token (not the PID, to avoid reuse ambiguity),
// takes the advisory session lock, and opens the encrypted DB. On any failure it
// removes whatever it created so a broken open never orphans a session directory,
// and the surfaced error is path-free.
func newBrowseOpen(cacheDir string, maxDiskBytes int64) func(context.Context) (app.BrowseStore, error) {
	return func(ctx context.Context) (app.BrowseStore, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("browse key: %w", err)
		}

		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return nil, fmt.Errorf("browse session id: %w", err)
		}
		dir := filepath.Join(cacheDir, browsedb.SessionPrefix+hex.EncodeToString(token))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("browse session dir: %w", browsedb.PathFreeFSError(err))
		}

		lock, err := browsedb.LockSession(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}

		db, err := browsedb.OpenContext(ctx, filepath.Join(dir, "db.sqlite"), key, maxDiskBytes)
		if err != nil {
			_ = lock.Close()
			_ = os.RemoveAll(dir)
			return nil, err
		}
		return &browseStore{db: db, lock: lock, dir: dir}, nil
	}
}
