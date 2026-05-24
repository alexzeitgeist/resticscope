package app

import (
	"context"
	"errors"
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
}

func (f fakeRestic) Snapshots(ctx context.Context, t resticx.Target, c resticx.Creds) ([]model.Snapshot, error) {
	return f.snaps, f.snapErr
}

func (f fakeRestic) CatConfig(ctx context.Context, t resticx.Target, c resticx.Creds) error {
	return f.catErr
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
