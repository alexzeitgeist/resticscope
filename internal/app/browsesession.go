package app

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Snapshot browsing streams repository metadata into a session-scoped encrypted
// store. Queries use that store without restic or secret resolution; filenames
// never enter Cache or RepoState, and the store is destroyed on clean exit.

// ErrBrowseNotEnabled indicates that App.Browse is nil.
var ErrBrowseNotEnabled = errors.New("browse is not enabled")

// ErrBrowseIncomplete indicates that indexing ended before the full namespace
// was emitted. The partial index is rolled back and never marked complete.
var ErrBrowseIncomplete = errors.New("browse index did not complete")

var errBrowseSessionClosed = errors.New("browse session is closed")

// browseProgressCheckEvery limits clock reads while retaining responsive progress.
const browseProgressCheckEvery = 2000

// browseProgressInterval caps live redraws; the stream end reports an exact count.
const browseProgressInterval = 250 * time.Millisecond

// browseMemoryTrimAfterEntries gates the post-index cost of returning idle heap
// spans to the OS.
const browseMemoryTrimAfterEntries = 250000

// BrowseStore provides session-scoped snapshot indexing and queries.
type BrowseStore interface {
	// IsIndexed reports whether snapshot has a committed index.
	IsIndexed(ctx context.Context, repo, snapshot string) (bool, error)
	// BeginIndex starts an index transaction for snapshot.
	BeginIndex(ctx context.Context, repo, snapshot string) (IndexWriter, error)
	// ListDir returns the entries immediately within dir.
	ListDir(ctx context.Context, repo, snapshot, dir string) ([]model.BrowseEntry, error)
	// SubtreeCounts returns descendant counts and whether an index is available.
	SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error)
	// Search returns up to limit fuzzy matches for query.
	Search(ctx context.Context, repo, snapshot, query string, limit int) (model.BrowseSearchResult, error)
	// Close releases store resources.
	Close() error
}

// IndexWriter is one index transaction. Commit marks the snapshot indexed only
// on success, and Rollback is idempotent.
type IndexWriter interface {
	// Add stores one snapshot node.
	Add(ctx context.Context, n model.BrowseNode) error
	// Count returns the number of nodes added.
	Count() int
	// Commit publishes the transaction and marks the snapshot indexed.
	Commit(ctx context.Context) error
	// Rollback discards the transaction; repeated calls are safe.
	Rollback() error
}

// BrowseSession owns the lazily opened store for one application run. It
// serializes store operations with Close so shutdown cannot close an active DB.
type BrowseSession struct {
	open func(context.Context) (BrowseStore, error)

	mu       sync.Mutex // protects store, closed, and inflight
	store    BrowseStore
	closed   bool
	inflight context.CancelFunc // cancels the current store op so Close can interrupt it

	// TUI shutdown does not wait for command goroutines, so Close cancels the
	// current operation before waiting on opMu to close the store.
	opMu sync.Mutex
}

// NewBrowseSession returns a session that calls open on first use. On error,
// open must clean up any resources it created.
func NewBrowseSession(open func(context.Context) (BrowseStore, error)) *BrowseSession {
	return &BrowseSession{open: open}
}

// ensureStore opens the session store on first use and does not cache failures.
// It drops mu during open so Close can cancel it; callers hold opMu to serialize
// opens.
func (s *BrowseSession) ensureStore(ctx context.Context) (BrowseStore, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errBrowseSessionClosed
	}
	if s.store != nil {
		store := s.store
		s.mu.Unlock()
		return store, nil
	}
	open := s.open
	s.mu.Unlock()

	store, err := open(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		closeErr := store.Close()
		return nil, errors.Join(err, closeErr)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		closeErr := store.Close()
		return nil, errors.Join(errBrowseSessionClosed, closeErr)
	}
	s.store = store
	return store, nil
}

// noopRelease lets beginOp return a callable release after it has already
// cleaned up an error path.
func noopRelease() {}

// beginOp locks the session and returns a child context cancellable by either the
// caller or Close. The always-callable release must be deferred; ctx and store
// are valid only when err is nil. A closed session returns errBrowseSessionClosed.
func (s *BrowseSession) beginOp(ctx context.Context) (context.Context, BrowseStore, func(), error) {
	s.opMu.Lock()
	opCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		s.opMu.Unlock()
		return nil, nil, noopRelease, errBrowseSessionClosed
	}
	s.inflight = cancel
	s.mu.Unlock()

	store, err := s.ensureStore(opCtx)
	if err != nil {
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		cancel()
		s.opMu.Unlock()
		return nil, nil, noopRelease, err
	}

	release := func() {
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		cancel()
		s.opMu.Unlock()
	}
	return opCtx, store, release, nil
}

// Close cancels any in-flight store op, waits for it to unwind, then closes the
// underlying store if it was ever opened. It is idempotent.
func (s *BrowseSession) Close() error {
	// Atomically marking closed and taking cancel makes a concurrent beginOp either
	// abort or register the operation that Close interrupts.
	s.mu.Lock()
	s.closed = true
	cancel := s.inflight
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil
	}
	err := s.store.Close()
	s.store = nil
	return err
}

// IndexSnapshot indexes a snapshot unless it is already indexed. It serializes
// the transaction with other session operations and Close, which can cancel the
// stream independently of the caller. Every failure rolls back and leaves the
// snapshot unmarked. A non-nil progress receives throttled counts and the exact
// final count.
func (a *App) IndexSnapshot(ctx context.Context, repoName, snapshotID string, progress func(n int)) error {
	if a.Browse == nil {
		return ErrBrowseNotEnabled
	}
	r, ok := a.repo(repoName)
	if !ok {
		return unknownRepoError(repoName)
	}
	// Schedule the stop-the-world trim before beginOp so it runs after release
	// unlocks the session, and only after committing a large index.
	trim := false
	defer func() {
		if trim {
			debug.FreeOSMemory()
		}
	}()

	ctx, store, release, err := a.Browse.beginOp(ctx)
	if err != nil {
		return err
	}
	defer release()

	indexed, err := store.IsIndexed(ctx, r.Name, snapshotID)
	if err != nil {
		return err
	}
	if indexed {
		return nil
	}

	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return err
	}

	tx, err := store.BeginIndex(ctx, r.Name, snapshotID)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback is safe after Commit has already rolled back a poisoned transaction.
			_ = tx.Rollback()
		}
	}()

	summary, err := a.Restic.StreamSnapshotTree(ctx, targetOf(r), resticCreds(material),
		snapshotID, a.Cfg.Browse.IndexTimeout.Std(), browseProgressFunc(ctx, tx, progress))
	if err != nil {
		// Callback and restic failures both leave the deferred rollback to discard tx.
		return err
	}
	if progress != nil {
		progress(tx.Count())
	}
	if !summary.IsComplete {
		return ErrBrowseIncomplete
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	trim = tx.Count() >= browseMemoryTrimAfterEntries
	return nil
}

// browseProgressFunc stores streamed nodes and emits throttled progress without
// reading the clock for every node.
func browseProgressFunc(ctx context.Context, tx IndexWriter, progress func(n int)) func(model.BrowseNode) error {
	var lastProgressAt time.Time
	return func(n model.BrowseNode) error {
		if err := tx.Add(ctx, n); err != nil {
			return err
		}
		if progress != nil {
			if c := tx.Count(); c%browseProgressCheckEvery == 0 {
				now := time.Now()
				if lastProgressAt.IsZero() || now.Sub(lastProgressAt) >= browseProgressInterval {
					lastProgressAt = now
					progress(c)
				}
			}
		}
		return nil
	}
}

// ListDir returns a directory's children without invoking restic or resolving secrets.
func (a *App) ListDir(ctx context.Context, repoName, snapshotID, dir string) ([]model.BrowseEntry, error) {
	if a.Browse == nil {
		return nil, ErrBrowseNotEnabled
	}
	r, ok := a.repo(repoName)
	if !ok {
		return nil, unknownRepoError(repoName)
	}
	ctx, store, release, err := a.Browse.beginOp(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return store.ListDir(ctx, r.Name, snapshotID, dir)
}

// SubtreeCounts returns advisory extraction-review counts without invoking
// restic or resolving secrets. A nil Browse session or unindexed snapshot
// reports known=false instead of an error.
func (a *App) SubtreeCounts(ctx context.Context, repoName, snapshotID, dir string) (files, dirs int, known bool, err error) {
	if a.Browse == nil {
		return 0, 0, false, nil
	}
	r, ok := a.repo(repoName)
	if !ok {
		return 0, 0, false, unknownRepoError(repoName)
	}
	ctx, store, release, err := a.Browse.beginOp(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	defer release()
	return store.SubtreeCounts(ctx, r.Name, snapshotID, dir)
}

// SearchSnapshot returns up to limit fuzzy filename matches from the session
// store without invoking restic or resolving secrets. Store errors are path-free.
func (a *App) SearchSnapshot(ctx context.Context, repoName, snapshotID, query string, limit int) (model.BrowseSearchResult, error) {
	if a.Browse == nil {
		return model.BrowseSearchResult{}, ErrBrowseNotEnabled
	}
	r, ok := a.repo(repoName)
	if !ok {
		return model.BrowseSearchResult{}, unknownRepoError(repoName)
	}
	ctx, store, release, err := a.Browse.beginOp(ctx)
	if err != nil {
		return model.BrowseSearchResult{}, err
	}
	defer release()
	return store.Search(ctx, r.Name, snapshotID, query, limit)
}
