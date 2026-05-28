package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"resticscope/internal/cache"
	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
)

// --- fakes ---

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type fakeCache struct {
	mu      sync.Mutex
	states  map[string]model.RepoState
	loadErr map[string]error
	saved   map[string]model.RepoState
	saveErr error // if set, every Save fails with it
}

func newFakeCache() *fakeCache {
	return &fakeCache{states: map[string]model.RepoState{}, loadErr: map[string]error{}, saved: map[string]model.RepoState{}}
}

func (f *fakeCache) Load(ctx context.Context, name string) (model.RepoState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.loadErr[name]; err != nil {
		return model.RepoState{}, err
	}
	st, ok := f.states[name]
	if !ok {
		return model.RepoState{}, cache.ErrMiss
	}
	return st, nil
}

func (f *fakeCache) Save(ctx context.Context, name string, state model.RepoState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved[name] = state
	return nil
}

type fakeSecrets struct{ err error }

func (f fakeSecrets) Resolve(repoName, credName string) (secrets.Material, error) {
	if f.err != nil {
		return secrets.Material{}, f.err
	}
	return secrets.Material{AccessKey: "AK", SecretKey: "SK", ResticPassword: "pw"}, nil
}

type fakeRestic struct {
	snaps   []model.Snapshot
	snapErr error
	catErr  error // returned by CatConfig

	browseNodes   []model.BrowseNode      // nodes streamed to onNode in order
	browseSummary model.BrowseScanSummary // returned on a clean stream (Entries overwritten with the count actually emitted)
	browseErr     error                   // returned instead of a summary (simulates a restic failure)
	browseDelay   time.Duration           // sleep after emitting nodes, widening the open-tx window for the serialization test
	browseCap     *browseCapture          // optional; records what StreamSnapshotTree was asked
}

// browseCapture records StreamSnapshotTree's arguments. It is a pointer field so
// the value-receiver fake (kept a value to satisfy the interface as the existing
// tests pass it by value) can still record through it; the mutex guards it when
// two goroutines share one fake in the serialization test.
type browseCapture struct {
	mu      sync.Mutex
	snapID  string
	target  resticx.Target
	timeout time.Duration
}

func (f fakeRestic) Snapshots(ctx context.Context, t resticx.Target, c resticx.Creds) ([]model.Snapshot, error) {
	return f.snaps, f.snapErr
}

func (f fakeRestic) CatConfig(ctx context.Context, t resticx.Target, c resticx.Creds) error {
	return f.catErr
}

func (f fakeRestic) StreamSnapshotTree(ctx context.Context, t resticx.Target, c resticx.Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	if f.browseCap != nil {
		f.browseCap.mu.Lock()
		f.browseCap.snapID = snapshotID
		f.browseCap.target = t
		f.browseCap.timeout = timeout
		f.browseCap.mu.Unlock()
	}
	n := 0
	for _, node := range f.browseNodes {
		if err := onNode(node); err != nil {
			// Mirror resticx: a callback (store/disk-limit/cancel) error is surfaced
			// verbatim so the app can classify it, not as a restic failure.
			return model.BrowseScanSummary{Entries: n}, err
		}
		n++
	}
	if f.browseDelay > 0 {
		time.Sleep(f.browseDelay)
	}
	if f.browseErr != nil {
		return model.BrowseScanSummary{Entries: n}, f.browseErr
	}
	summary := f.browseSummary
	summary.Entries = n
	return summary, nil
}

type blockingBrowseRestic struct {
	started chan struct{}
}

func (blockingBrowseRestic) Snapshots(ctx context.Context, t resticx.Target, c resticx.Creds) ([]model.Snapshot, error) {
	return nil, nil
}

func (blockingBrowseRestic) CatConfig(ctx context.Context, t resticx.Target, c resticx.Creds) error {
	return nil
}

func (r blockingBrowseRestic) StreamSnapshotTree(ctx context.Context, t resticx.Target, c resticx.Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	close(r.started)
	<-ctx.Done()
	return model.BrowseScanSummary{}, ctx.Err()
}

// --- fake browse store / index writer ---

func storeKey(repo, snap string) string { return repo + "\x00" + snap }

// fakeStore is an in-memory BrowseStore. It records the repo keys it is asked
// about (to prove the app passes the configured repo name, not a target), tracks
// commit/rollback/indexed state per snapshot, and counts the maximum number of
// concurrently-open index transactions (which the session operation lock must hold
// to 1).
type fakeStore struct {
	mu sync.Mutex

	indexed map[string]bool                // snapshots with a committed index
	entries map[string][]model.BrowseEntry // ListDir results, keyed storeKey+"\x00"+dir

	isIndexedErr error
	beginErr     error
	listErr      error

	addErrAt  int   // if >0, the writer's Add fails on this call number
	addErr    error // error Add returns at addErrAt
	commitErr error

	repos      []string // every repo arg seen across calls
	beginCalls int
	committed  map[string]bool
	rolledBack map[string]bool
	closed     bool

	concurrent int // currently-open index transactions
	maxConc    int // high-water mark
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		indexed:    map[string]bool{},
		entries:    map[string][]model.BrowseEntry{},
		committed:  map[string]bool{},
		rolledBack: map[string]bool{},
	}
}

func (s *fakeStore) IsIndexed(ctx context.Context, repo, snap string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = append(s.repos, repo)
	if s.isIndexedErr != nil {
		return false, s.isIndexedErr
	}
	return s.indexed[storeKey(repo, snap)], nil
}

func (s *fakeStore) BeginIndex(ctx context.Context, repo, snap string) (IndexWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = append(s.repos, repo)
	s.beginCalls++
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	s.concurrent++
	if s.concurrent > s.maxConc {
		s.maxConc = s.concurrent
	}
	return &fakeWriter{store: s, repo: repo, snap: snap, addErrAt: s.addErrAt, addErr: s.addErr, commitErr: s.commitErr}, nil
}

func (s *fakeStore) ListDir(ctx context.Context, repo, snap, dir string) ([]model.BrowseEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = append(s.repos, repo)
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.entries[storeKey(repo, snap)+"\x00"+dir], nil
}

func (s *fakeStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// fakeWriter is one index transaction over fakeStore.
type fakeWriter struct {
	store      *fakeStore
	repo, snap string
	addErrAt   int
	addErr     error
	commitErr  error

	added  []model.BrowseNode
	count  int
	closed bool // Commit or Rollback has settled this tx (decrements concurrency once)
}

func (w *fakeWriter) Add(ctx context.Context, n model.BrowseNode) error {
	w.count++
	if w.addErrAt > 0 && w.count >= w.addErrAt {
		return w.addErr
	}
	w.added = append(w.added, n)
	return nil
}

func (w *fakeWriter) Count() int { return w.count }

func (w *fakeWriter) Commit(ctx context.Context) error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if !w.closed {
		w.store.concurrent--
		w.closed = true
	}
	if w.commitErr != nil {
		w.store.rolledBack[storeKey(w.repo, w.snap)] = true
		return w.commitErr
	}
	w.store.indexed[storeKey(w.repo, w.snap)] = true
	w.store.committed[storeKey(w.repo, w.snap)] = true
	return nil
}

func (w *fakeWriter) Rollback() error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if !w.closed {
		w.store.concurrent--
		w.closed = true
	}
	w.store.rolledBack[storeKey(w.repo, w.snap)] = true
	return nil
}

// browseApp wires an App whose Browse session serves store and whose Restic is r.
func browseApp(store BrowseStore, r fakeRestic) *App {
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: r}
	a.Browse = NewBrowseSession(func() (BrowseStore, error) { return store, nil })
	return a
}

// --- helpers ---

func testConfig() *config.Config {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 4},
		Credentials: []config.Credential{
			{Name: "cred-a"},
		},
		Repos: []config.Repo{
			{Name: "repo-a", Credential: "cred-a", Endpoint: "https://fsn1.example.com", Region: "fsn1", BucketLookup: "auto", Bucket: "bucket-a", ExpectedFrequency: config.Duration(24 * time.Hour)},
		},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.LockMaxAge = config.Duration(30 * time.Minute)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	return cfg
}

var now = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

// --- tests ---

func TestStatusesColdCacheIsGrey(t *testing.T) {
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}}
	rows, err := a.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != model.StatusGrey {
		t.Fatalf("expected one grey row, got %+v", rows)
	}
}

func TestStatusesEvaluatesAndFlagsStale(t *testing.T) {
	fc := newFakeCache()
	fc.states["repo-a"] = model.RepoState{
		Name:         "repo-a",
		RefreshedAt:  now.Add(-time.Hour), // older than stale_after (10m)
		LastSnapshot: now.Add(-2 * time.Hour),
	}
	a := &App{Cfg: testConfig(), Cache: fc, Clock: fixedClock{now}}
	rows, err := a.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if rows[0].Status != model.StatusGreen {
		t.Errorf("status = %v, want green", rows[0].Status)
	}
	if !rows[0].Stale {
		t.Error("expected Stale = true for an old cache entry")
	}
}

func TestStatusesPropagatesRealCacheError(t *testing.T) {
	fc := newFakeCache()
	fc.loadErr["repo-a"] = context.Canceled
	a := &App{Cfg: testConfig(), Cache: fc, Clock: fixedClock{now}}
	if _, err := a.Statuses(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled to propagate, got %v", err)
	}
}

func TestRefreshSuccess(t *testing.T) {
	fc := newFakeCache()
	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic: fakeRestic{
			snaps: []model.Snapshot{
				{Hostname: "homeserver", Time: now.Add(-2 * time.Hour), Tags: []string{"daily"}},
			},
		},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state.Status != model.StatusGreen {
		t.Errorf("status = %v, want green", state.Status)
	}
	if state.SnapshotCount != 1 {
		t.Errorf("unexpected state: %+v", state)
	}
	if len(state.Hosts) != 1 || state.Hosts[0] != "homeserver" {
		t.Errorf("observed hosts = %v", state.Hosts)
	}
	if got := fc.saved["repo-a"]; got.Status != model.StatusGreen {
		t.Errorf("state was not persisted: %+v", got)
	}
}

func TestRefreshResticErrorRecorded(t *testing.T) {
	fc := newFakeCache()
	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snapErr: errors.New("restic snapshots: repository is locked (exit 11)")},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh should not return restic failure as error: %v", err)
	}
	if state.Status != model.StatusError {
		t.Errorf("status = %v, want error", state.Status)
	}
	if state.LastError == "" {
		t.Error("expected LastError to be recorded")
	}
	if _, ok := fc.saved["repo-a"]; !ok {
		t.Error("failed refresh should still be cached")
	}
}

// A transient snapshots failure must not erase the last-known-good observation:
// the repo reports StatusError (live verdict) but keeps the snapshots,
// last-snapshot time, and observed hosts/tags from the prior successful refresh,
// and keeps that refresh's RefreshedAt rather than advancing it.
func TestRefreshPreservesLastGoodOnFailure(t *testing.T) {
	fc := newFakeCache()
	good := model.RepoState{
		Name:          "repo-a",
		RefreshedAt:   now.Add(-2 * time.Hour),
		Status:        model.StatusGreen,
		SnapshotCount: 2,
		LastSnapshot:  now.Add(-3 * time.Hour),
		Snapshots:     []model.Snapshot{{Hostname: "homeserver"}, {Hostname: "homeserver"}},
		Hosts:         []string{"homeserver"},
		Tags:          []string{"daily"},
	}
	fc.states["repo-a"] = good

	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snapErr: errors.New("restic snapshots: repository does not exist (exit 10)")},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state.Status != model.StatusError || state.LastError == "" {
		t.Errorf("want live error verdict, got status=%v lastErr=%q", state.Status, state.LastError)
	}
	if state.SnapshotCount != 2 || len(state.Snapshots) != 2 {
		t.Errorf("snapshots not preserved: count=%d len=%d", state.SnapshotCount, len(state.Snapshots))
	}
	if !state.LastSnapshot.Equal(good.LastSnapshot) {
		t.Errorf("LastSnapshot = %v, want preserved %v", state.LastSnapshot, good.LastSnapshot)
	}
	if len(state.Hosts) != 1 || len(state.Tags) != 1 {
		t.Errorf("observed hosts/tags not preserved: hosts=%v tags=%v", state.Hosts, state.Tags)
	}
	if !state.RefreshedAt.Equal(good.RefreshedAt) {
		t.Errorf("RefreshedAt = %v, want preserved last-success %v", state.RefreshedAt, good.RefreshedAt)
	}
	if got := fc.saved["repo-a"]; got.Status != model.StatusError || got.SnapshotCount != 2 {
		t.Errorf("preserved state not persisted: %+v", got)
	}
}

// With no prior good state, a failed refresh stays an empty error state and —
// per the RefreshedAt = "last successful observation" semantics — leaves
// RefreshedAt zero so it reads as "last refresh failed", not "refreshed now".
func TestRefreshFailureWithNoPriorLeavesUnrefreshed(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(), // cold: no prior entry
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snapErr: errors.New("restic snapshots: repository does not exist (exit 10)")},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state.Status != model.StatusError || state.LastError == "" {
		t.Errorf("expected recorded error, got %+v", state)
	}
	if state.SnapshotCount != 0 || len(state.Snapshots) != 0 {
		t.Errorf("expected no snapshots with no prior good state, got %+v", state)
	}
	if !state.RefreshedAt.IsZero() {
		t.Errorf("RefreshedAt = %v, want zero (never successfully refreshed)", state.RefreshedAt)
	}
}

// RefreshAll honors the same preservation: a per-repo snapshots failure keeps
// that repo's last-known-good data while reporting it as an error.
func TestRefreshAllPreservesLastGoodOnFailure(t *testing.T) {
	fc := newFakeCache()
	fc.states["repo-a"] = model.RepoState{
		Name:          "repo-a",
		RefreshedAt:   now.Add(-2 * time.Hour),
		Status:        model.StatusGreen,
		SnapshotCount: 3,
		LastSnapshot:  now.Add(-90 * time.Minute),
		Snapshots:     []model.Snapshot{{Hostname: "homeserver"}, {Hostname: "homeserver"}, {Hostname: "homeserver"}},
		Hosts:         []string{"homeserver"},
	}
	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snapErr: errors.New("restic snapshots: repository does not exist (exit 10)")},
	}
	results, err := a.RefreshAll(context.Background())
	if err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	got := results[0]
	if got.Status != model.StatusError || got.LastError == "" {
		t.Errorf("want live error verdict, got %+v", got)
	}
	if got.SnapshotCount != 3 || !got.LastSnapshot.Equal(now.Add(-90*time.Minute)) {
		t.Errorf("last-known-good not preserved through RefreshAll: %+v", got)
	}
	if saved := fc.saved["repo-a"]; saved.SnapshotCount != 3 || saved.Status != model.StatusError {
		t.Errorf("preserved state not persisted: %+v", saved)
	}
}

func TestRefreshSecretsErrorRecorded(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{err: errors.New("secrets: no repo \"repo-a\"")},
		Restic:  fakeRestic{},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state.Status != model.StatusError || state.LastError == "" {
		t.Errorf("expected recorded secrets error, got %+v", state)
	}
}

func TestRefreshAll(t *testing.T) {
	cfg := testConfig()
	cfg.Repos = append(cfg.Repos,
		config.Repo{Name: "repo-b", Credential: "cred-a", Endpoint: "https://fsn1.example.com", Bucket: "bucket-b", ExpectedFrequency: config.Duration(24 * time.Hour)},
		config.Repo{Name: "repo-c", Credential: "cred-a", Endpoint: "https://fsn1.example.com", Bucket: "bucket-c", ExpectedFrequency: config.Duration(24 * time.Hour)},
	)
	fc := newFakeCache()
	a := &App{
		Cfg:     cfg,
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snaps: []model.Snapshot{{Hostname: "h", Time: now.Add(-time.Hour)}}},
	}
	results, err := a.RefreshAll(context.Background())
	if err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if results[i].Name != name {
			t.Errorf("result[%d].Name = %q, want %q (order not preserved)", i, results[i].Name, name)
		}
		if _, ok := fc.saved[name]; !ok {
			t.Errorf("%q not persisted", name)
		}
	}
}

// A failed cache write must surface as an error AND the returned states must be
// the live refresh, never the (possibly green) stale cache. This guards the
// `status --refresh` contract: refresh means live state, even when the disk is
// unwritable.
func TestRefreshAllSurfacesSaveFailureWithLiveState(t *testing.T) {
	fc := newFakeCache()
	// An old green cache entry that must NOT be what we report after refresh.
	fc.states["repo-a"] = model.RepoState{Name: "repo-a", RefreshedAt: now.Add(-time.Hour), LastSnapshot: now.Add(-2 * time.Hour)}
	fc.saveErr = errors.New("disk full")

	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snapErr: errors.New("restic snapshots: repository is locked (exit 11)")},
	}
	states, err := a.RefreshAll(context.Background())
	if err == nil {
		t.Fatal("expected RefreshAll to surface the cache save failure")
	}
	if states[0].Status != model.StatusError {
		t.Errorf("status = %v, want error (live), not stale green", states[0].Status)
	}
	if rows := a.RowsFromStates(states); WorstExitCode(rows) != 2 {
		t.Errorf("exit code from live rows = %d, want 2 despite the save failure", WorstExitCode(a.RowsFromStates(states)))
	}
}

func TestRefreshRow(t *testing.T) {
	fc := newFakeCache()
	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic: fakeRestic{
			snaps: []model.Snapshot{{Hostname: "homeserver", Time: now.Add(-2 * time.Hour)}},
		},
	}
	row, err := a.RefreshRow(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("RefreshRow: %v", err)
	}
	if row.Name != "repo-a" || row.Status != model.StatusGreen {
		t.Errorf("row = %+v, want green repo-a", row)
	}
	if row.State.SnapshotCount != 1 {
		t.Errorf("row.State not the live refresh: %+v", row.State)
	}
}

// A save failure must surface as an error but still return the live row, so the
// TUI shows fresh state with a warning rather than falling back to stale cache.
func TestRefreshRowSurfacesSaveFailureWithLiveRow(t *testing.T) {
	fc := newFakeCache()
	fc.saveErr = errors.New("disk full")
	a := &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{snaps: []model.Snapshot{{Hostname: "homeserver", Time: now.Add(-2 * time.Hour)}}},
	}
	row, err := a.RefreshRow(context.Background(), "repo-a")
	if err == nil {
		t.Fatal("expected the save failure to be surfaced")
	}
	if row.Status != model.StatusGreen {
		t.Errorf("status = %v, want live green despite save failure", row.Status)
	}
}

func TestRefreshRowUnknownRepo(t *testing.T) {
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}}
	if _, err := a.RefreshRow(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestIndexSnapshotIndexesAllNodes(t *testing.T) {
	store := newFakeStore()
	bc := &browseCapture{}
	nodes := []model.BrowseNode{
		{Path: "/home", Name: "home", IsDir: true},
		{Path: "/home/notes.txt", Name: "notes.txt", Size: 7},
		{Path: "/etc", Name: "etc", IsDir: true},
	}
	a := browseApp(store, fakeRestic{browseNodes: nodes, browseSummary: model.BrowseScanSummary{Complete: true}, browseCap: bc})

	var lastProgress int
	if err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", func(n int) { lastProgress = n }); err != nil {
		t.Fatalf("IndexSnapshot: %v", err)
	}
	// Resolved the right snapshot, the repo's target, and the configured timeout.
	if bc.snapID != "snap123" {
		t.Errorf("snapshot ID = %q, want snap123", bc.snapID)
	}
	if bc.target.Name != "repo-a" || bc.target.Bucket != "bucket-a" {
		t.Errorf("target = %+v, want repo-a/bucket-a", bc.target)
	}
	if bc.timeout != 10*time.Minute {
		t.Errorf("timeout = %v, want 10m (cfg.Browse.IndexTimeout)", bc.timeout)
	}
	// Committed and marked indexed; every node was streamed into the writer.
	if !store.indexed[storeKey("repo-a", "snap123")] {
		t.Error("snapshot not marked indexed after a complete stream")
	}
	if !store.committed[storeKey("repo-a", "snap123")] {
		t.Error("index transaction was not committed")
	}
	if store.rolledBack[storeKey("repo-a", "snap123")] {
		t.Error("a complete stream must not roll back")
	}
	if lastProgress != len(nodes) {
		t.Errorf("final progress = %d, want %d", lastProgress, len(nodes))
	}
	// The store is keyed by the configured repo NAME, never the bucket/endpoint.
	for _, r := range store.repos {
		if r != "repo-a" {
			t.Errorf("store saw repo key %q, want the configured name repo-a", r)
		}
	}
}

func TestIndexSnapshotIdempotent(t *testing.T) {
	store := newFakeStore()
	store.indexed[storeKey("repo-a", "snap123")] = true
	bc := &browseCapture{}
	a := browseApp(store, fakeRestic{browseNodes: []model.BrowseNode{{Path: "/x", Name: "x"}}, browseSummary: model.BrowseScanSummary{Complete: true}, browseCap: bc})

	if err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", nil); err != nil {
		t.Fatalf("IndexSnapshot on already-indexed: %v", err)
	}
	if store.beginCalls != 0 {
		t.Errorf("BeginIndex called %d times, want 0 for an already-indexed snapshot", store.beginCalls)
	}
	if bc.snapID != "" {
		t.Error("restic must not be invoked for an already-indexed snapshot")
	}
}

func TestIndexSnapshotIncompleteRollsBack(t *testing.T) {
	store := newFakeStore()
	a := browseApp(store, fakeRestic{
		browseNodes:   []model.BrowseNode{{Path: "/a", Name: "a"}, {Path: "/b", Name: "b"}},
		browseSummary: model.BrowseScanSummary{Complete: false},
	})
	err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", nil)
	if !errors.Is(err, ErrBrowseIncomplete) {
		t.Fatalf("err = %v, want ErrBrowseIncomplete", err)
	}
	if store.indexed[storeKey("repo-a", "snap123")] {
		t.Error("an incomplete stream must not be marked indexed")
	}
	if !store.rolledBack[storeKey("repo-a", "snap123")] {
		t.Error("an incomplete stream must roll back")
	}
}

func TestIndexSnapshotDiskLimitRollsBack(t *testing.T) {
	store := newFakeStore()
	store.addErrAt = 2 // the store rejects the 2nd Add with a path-free disk-limit error
	store.addErr = fmt.Errorf("browsedb add: %w", model.ErrBrowseDiskLimit)
	a := browseApp(store, fakeRestic{
		browseNodes:   []model.BrowseNode{{Path: "/home/secret-passwords.txt", Name: "secret-passwords.txt"}, {Path: "/home/more-secrets.txt", Name: "more-secrets.txt"}},
		browseSummary: model.BrowseScanSummary{Complete: true},
	})
	err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", nil)
	if !errors.Is(err, model.ErrBrowseDiskLimit) {
		t.Fatalf("err = %v, want it to wrap ErrBrowseDiskLimit", err)
	}
	if store.indexed[storeKey("repo-a", "snap123")] {
		t.Error("a disk-limited index must not be marked indexed")
	}
	if !store.rolledBack[storeKey("repo-a", "snap123")] {
		t.Error("a disk-limited index must roll back")
	}
	// The surfaced error must never leak a filename.
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("surfaced error leaked a filename: %q", err.Error())
	}
}

func TestIndexSnapshotResticErrorRollsBack(t *testing.T) {
	store := newFakeStore()
	a := browseApp(store, fakeRestic{
		browseNodes: []model.BrowseNode{{Path: "/a", Name: "a"}},
		browseErr:   errors.New("restic ls: repository is locked (exit 11)"),
	})
	if err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", nil); err == nil {
		t.Fatal("expected the restic failure to surface")
	}
	if store.indexed[storeKey("repo-a", "snap123")] {
		t.Error("a failed stream must not be marked indexed")
	}
	if !store.rolledBack[storeKey("repo-a", "snap123")] {
		t.Error("a failed stream must roll back")
	}
}

func TestIndexSnapshotSecretsErrorPropagates(t *testing.T) {
	store := newFakeStore()
	a := browseApp(store, fakeRestic{})
	a.Secrets = fakeSecrets{err: errors.New("secrets: no repo \"repo-a\"")}
	if err := a.IndexSnapshot(context.Background(), "repo-a", "snap123", nil); err == nil {
		t.Fatal("expected secrets error to propagate")
	}
	if store.beginCalls != 0 {
		t.Errorf("BeginIndex called %d times, want 0 when secrets fail before indexing", store.beginCalls)
	}
}

func TestIndexSnapshotSerializedBySessionOperationLock(t *testing.T) {
	store := newFakeStore()
	a := browseApp(store, fakeRestic{
		browseNodes:   []model.BrowseNode{{Path: "/a", Name: "a"}, {Path: "/b", Name: "b"}},
		browseSummary: model.BrowseScanSummary{Complete: true},
		browseDelay:   10 * time.Millisecond, // hold each tx open long enough to overlap if unserialized
	})
	var wg sync.WaitGroup
	for _, snap := range []string{"snapA", "snapB"} {
		wg.Add(1)
		go func(snap string) {
			defer wg.Done()
			if err := a.IndexSnapshot(context.Background(), "repo-a", snap, nil); err != nil {
				t.Errorf("IndexSnapshot(%s): %v", snap, err)
			}
		}(snap)
	}
	wg.Wait()
	if store.maxConc > 1 {
		t.Errorf("max concurrent index transactions = %d, want 1 (session operation lock must serialize)", store.maxConc)
	}
	if !store.indexed[storeKey("repo-a", "snapA")] || !store.indexed[storeKey("repo-a", "snapB")] {
		t.Error("both snapshots should be indexed after serialized runs")
	}
}

func TestBrowseSessionCloseWaitsForInFlightIndex(t *testing.T) {
	store := newFakeStore()
	started := make(chan struct{})
	a := browseApp(store, fakeRestic{})
	a.Restic = blockingBrowseRestic{started: started}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	indexDone := make(chan error, 1)
	go func() {
		indexDone <- a.IndexSnapshot(ctx, "repo-a", "snap123", nil)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("index never reached the blocking restic stream")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- a.Browse.Close()
	}()

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the in-flight index was cancelled and unwound: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-indexDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("IndexSnapshot err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("index did not exit after cancellation")
	}

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the index exited")
	}

	store.mu.Lock()
	closed := store.closed
	concurrent := store.concurrent
	rolledBack := store.rolledBack[storeKey("repo-a", "snap123")]
	store.mu.Unlock()
	if !closed {
		t.Fatal("store was not closed")
	}
	if concurrent != 0 {
		t.Fatalf("index transaction still open after Close: %d", concurrent)
	}
	if !rolledBack {
		t.Fatal("in-flight index was not rolled back before Close returned")
	}
	if _, err := a.ListDir(context.Background(), "repo-a", "snap123", "/"); !errors.Is(err, errBrowseSessionClosed) {
		t.Fatalf("ListDir after Close err = %v, want errBrowseSessionClosed", err)
	}
}

func TestIndexSnapshotNilBrowseGuard(t *testing.T) {
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: fakeRestic{}}
	if err := a.IndexSnapshot(context.Background(), "repo-a", "s1", nil); !errors.Is(err, ErrBrowseNotEnabled) {
		t.Fatalf("err = %v, want ErrBrowseNotEnabled when Browse is nil", err)
	}
}

func TestIndexSnapshotUnknownRepo(t *testing.T) {
	a := browseApp(newFakeStore(), fakeRestic{})
	if err := a.IndexSnapshot(context.Background(), "nope", "s1", nil); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestEnsureStoreOpenFailureNotCached(t *testing.T) {
	calls := 0
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: fakeRestic{}}
	a.Browse = NewBrowseSession(func() (BrowseStore, error) {
		calls++
		return nil, errors.New("open failed")
	})
	if err := a.IndexSnapshot(context.Background(), "repo-a", "s1", nil); err == nil {
		t.Fatal("expected the open failure to surface")
	}
	if err := a.IndexSnapshot(context.Background(), "repo-a", "s1", nil); err == nil {
		t.Fatal("expected the open failure to surface on retry too")
	}
	if calls != 2 {
		t.Errorf("open called %d times, want 2 (a failed open must not be cached)", calls)
	}
}

func TestListDirDelegates(t *testing.T) {
	store := newFakeStore()
	want := []model.BrowseEntry{{Path: "/home/notes.txt", Name: "notes.txt", Size: 7}}
	store.entries[storeKey("repo-a", "snap123")+"\x00/home"] = want
	a := browseApp(store, fakeRestic{})

	got, err := a.ListDir(context.Background(), "repo-a", "snap123", "/home")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(got) != 1 || got[0].Name != "notes.txt" {
		t.Errorf("ListDir = %+v, want the store's rows", got)
	}
	for _, r := range store.repos {
		if r != "repo-a" {
			t.Errorf("store saw repo key %q, want repo-a", r)
		}
	}
}

func TestListDirNilBrowseGuard(t *testing.T) {
	a := &App{Cfg: testConfig(), Cache: newFakeCache(), Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: fakeRestic{}}
	if _, err := a.ListDir(context.Background(), "repo-a", "s1", "/"); !errors.Is(err, ErrBrowseNotEnabled) {
		t.Fatalf("err = %v, want ErrBrowseNotEnabled when Browse is nil", err)
	}
}

func TestListDirUnknownRepo(t *testing.T) {
	a := browseApp(newFakeStore(), fakeRestic{})
	if _, err := a.ListDir(context.Background(), "nope", "s1", "/"); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestWorstExitCode(t *testing.T) {
	tests := []struct {
		name     string
		statuses []model.Status
		want     int
	}{
		{"all green", []model.Status{model.StatusGreen, model.StatusGreen}, 0},
		{"amber bumps to 1", []model.Status{model.StatusGreen, model.StatusAmber}, 1},
		{"red is 2", []model.Status{model.StatusGreen, model.StatusRed}, 2},
		{"error is 2", []model.Status{model.StatusError}, 2},
		{"grey is 2", []model.Status{model.StatusGrey}, 2},
		{"red wins over amber", []model.Status{model.StatusAmber, model.StatusRed}, 2},
		{"empty is 0", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := make([]RepoStatus, len(tt.statuses))
			for i, s := range tt.statuses {
				rows[i] = RepoStatus{Status: s}
			}
			if got := WorstExitCode(rows); got != tt.want {
				t.Errorf("WorstExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}
