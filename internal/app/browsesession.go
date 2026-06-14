package app

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"resticscope/internal/model"
)

// browsesession.go orchestrates the in-app snapshot file browser on top of a
// session-scoped, encrypted browse store. IndexSnapshot resolves a repo's
// credentials (exactly like refreshOne), streams one recursive `restic ls`
// through resticx, and persists every node into the store inside a single index
// transaction — committing only on a clean, complete pass and rolling back on any
// cancel/error/incomplete/disk-limit. ListDir then serves navigation from the
// store alone, with no restic call and no secret resolution. Nothing here is
// written to Cache or RepoState; filenames live only in the encrypted store,
// which is destroyed on clean exit.

// ErrBrowseNotEnabled is returned by the browse methods when App.Browse is nil
// (non-TUI entry points never open a browse store).
var ErrBrowseNotEnabled = errors.New("browse is not enabled")

// ErrBrowseIncomplete is returned by IndexSnapshot when the stream ended before
// the whole namespace was emitted (e.g. the index timeout fired). The snapshot is
// rolled back and never marked indexed, so a truncated tree is never presented as
// complete; the caller can retry or fall back to the scoped shell.
var ErrBrowseIncomplete = errors.New("browse index did not complete")

var errBrowseSessionClosed = errors.New("browse session is closed")

// browseProgressCheckEvery avoids consulting the clock per node while keeping
// progress responsive enough on very large snapshots.
const browseProgressCheckEvery = 2000

// browseProgressInterval caps TUI progress redraws. The exact final count is
// emitted once the stream ends (on both the commit and incomplete paths), so this
// is only a live-display throttle.
const browseProgressInterval = 250 * time.Millisecond

// browseMemoryTrimAfterEntries is the point where the browse indexer has likely
// grown the Go heap enough that returning idle spans to the OS is worth the
// post-index GC cost.
const browseMemoryTrimAfterEntries = 250000

// BrowseStore is the consumer-side seam for the encrypted browse store. It is
// satisfied by a thin cmd-level wrapper over *browsedb.DB, keeping app free of the
// store package and easy to fake in tests.
type BrowseStore interface {
	IsIndexed(ctx context.Context, repo, snapshot string) (bool, error)
	BeginIndex(ctx context.Context, repo, snapshot string) (IndexWriter, error)
	ListDir(ctx context.Context, repo, snapshot, dir string) ([]model.BrowseEntry, error)
	SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error)
	Search(ctx context.Context, repo, snapshot, query string, limit int) (model.BrowseSearchResult, error)
	Close() error
}

// IndexWriter is one index transaction. Add buffers a node, Count reports
// progress, Commit is terminal (marks the snapshot indexed only on success), and
// Rollback is safe/idempotent.
type IndexWriter interface {
	Add(ctx context.Context, n model.BrowseNode) error
	Count() int
	Commit(ctx context.Context) error
	Rollback() error
}

// BrowseSession owns the lazily-created store for one app run. The store is
// opened on the first browse and reused for the rest of the session. The session
// operation lock serializes store use with Close so shutdown cannot tear down the
// DB under an in-flight browse command.
type BrowseSession struct {
	open func(context.Context) (BrowseStore, error)

	mu       sync.Mutex // protects store, closed, and inflight
	store    BrowseStore
	closed   bool
	inflight context.CancelFunc // cancels the current store op so Close can interrupt it

	// opMu serializes every store operation with Close so shutdown never tears down
	// the DB under an in-flight index/list. Bubble Tea does not wait for long-running
	// Cmd goroutines on shutdown, so Close does not rely on the caller having
	// cancelled the operation's context: it actively cancels the in-flight op (via
	// inflight) and then waits on opMu for it to unwind and release the store before
	// closing the SQLite pool and removing the session directory.
	opMu sync.Mutex
}

// NewBrowseSession returns a session whose store is opened lazily by open on the
// first browse. open is responsible for cleaning up anything it created if it
// returns an error, so a failed open leaves no orphaned session directory.
func NewBrowseSession(open func(context.Context) (BrowseStore, error)) *BrowseSession {
	return &BrowseSession{open: open}
}

// ensureStore returns the session store, opening it on first use. A failed open
// is not cached: the next browse retries rather than wedging on a broken store.
// It drops mu during open so Close can cancel a slow first open; callers must
// hold opMu, which serializes opens.
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

// noopRelease is the release func beginOp hands back on its error paths, so a
// caller may defer release() before checking err without risking a nil-func call.
// The op cleanup already ran inline on those paths, so this deliberately does
// nothing — calling the real release there would double-unlock opMu.
func noopRelease() {}

// beginOp acquires the session operation lock and returns the store together with
// a context Close can cancel. The returned context is a child of ctx, so the op
// still observes the caller's cancellation; registering it as the session's
// in-flight op additionally lets Close interrupt the op without depending on the
// caller. The returned release func unregisters the op and releases the lock, and
// MUST be deferred; it is always non-nil and safe to call (a no-op on the error
// paths), but ctx and store are only valid when err is nil. A closed session
// returns errBrowseSessionClosed.
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
	// Mark closed and grab the in-flight op's cancel together under mu, to ensure a
	// concurrent beginOp either observes closed (and aborts) or has already
	// registered its cancel here (and we interrupt it). Cancel outside the lock,
	// then wait on opMu for the op to release the store.
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

// IndexSnapshot indexes a snapshot's whole namespace into the session store,
// short-circuiting if it is already indexed. It holds the session operation lock
// across the whole IsIndexed → BeginIndex → stream → Commit/Rollback sequence so
// two concurrent callers cannot open two index transactions on the one DB, and
// Close cannot tear down the DB mid-write. The op runs under a context Close can
// cancel, so shutdown interrupts a long stream even if the caller never cancels.
// The stream is rolled back on every path until Commit succeeds; an incomplete
// stream (timeout) or any store/disk-limit/restic error leaves the snapshot
// unmarked. progress, if non-nil, is called with the running node count on a coarse
// cadence and once more with the exact final count when the stream ends.
func (a *App) IndexSnapshot(ctx context.Context, repoName, snapshotID string, progress func(n int)) error {
	if a.Browse == nil {
		return ErrBrowseNotEnabled
	}
	r, ok := a.repo(repoName)
	if !ok {
		return unknownRepoError(repoName)
	}
	// debug.FreeOSMemory() is a stop-the-world GC. Register the trim BEFORE beginOp
	// so it runs LAST (after release has dropped opMu), since running it under the session
	// lock would stall a concurrent Close/ListDir for the whole pause. It is gated
	// on a committed large index only — on a cancel/error path the tx is discarded
	// and ordinary GC reclaims the garbage, so a STW pause there would just stutter
	// interactive navigation for no lasting benefit.
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
			// Rollback is idempotent; safe even after Commit already rolled back a
			// poisoned tx.
			_ = tx.Rollback()
		}
	}()

	summary, err := a.Restic.StreamSnapshotTree(ctx, targetOf(r), resticCreds(material),
		snapshotID, a.Cfg.Browse.IndexTimeout.Std(), browseProgressFunc(ctx, tx, progress))
	if err != nil {
		// Includes the verbatim onNode error (store/disk-limit/cancel) and restic
		// failures; all leave the deferred rollback to discard the tx.
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

// browseProgressFunc builds the per-node callback that StreamSnapshotTree drives:
// it buffers each node into tx and, when progress is non-nil, reports the running
// count. It samples the clock only every browseProgressCheckEvery nodes and
// throttles to browseProgressInterval, to avoid a time.Now() call per node.
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

// ListDir returns one directory's children from the session store. It performs no
// restic call and resolves no secrets — navigation is pure SQL.
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

// SubtreeCounts reports how many files and directories dir contains within the
// indexed snapshot — display-only preflight detail for the extract review
// screen. Like ListDir it performs no restic call and resolves no secrets. It
// degrades rather than fails where it can: a nil Browse session reports
// known=false (a detail-view extract may run with browse disabled), and a
// never-indexed snapshot does the same via the store, so callers simply render
// no counts instead of an error.
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

// SearchSnapshot returns the best fuzzy filename matches for query anywhere in the
// indexed snapshot, capped to limit. Like ListDir it performs no restic call and
// resolves no secrets — the search runs entirely against the session store, which
// ranks the matches with the pure model scorer. App stays a thin delegate so the
// TUI only renders the result; the store's error is already path-free.
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
