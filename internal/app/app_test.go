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
	stats   model.Stats
	statErr error
}

func (f fakeRestic) Snapshots(ctx context.Context, t resticx.Target, c resticx.Creds) ([]model.Snapshot, error) {
	return f.snaps, f.snapErr
}

func (f fakeRestic) Stats(ctx context.Context, t resticx.Target, c resticx.Creds) (model.Stats, error) {
	return f.stats, f.statErr
}

// --- helpers ---

func testConfig() *config.Config {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 4},
		Credentials: []config.Credential{
			{Name: "cred-a", Endpoint: "https://fsn1.example.com", Region: "fsn1", BucketLookup: "auto"},
		},
		Repos: []config.Repo{
			{Name: "repo-a", Credential: "cred-a", Bucket: "bucket-a", ExpectedFrequency: config.Duration(24 * time.Hour), ExpectedHosts: []string{"homeserver"}},
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
				{Hostname: "homeserver", Time: now.Add(-2 * time.Hour), Paths: []string{"/etc"}, Tags: []string{"daily"}},
			},
			stats: model.Stats{TotalSize: 1000, TotalBlobCount: 42, SnapshotsCount: 1},
		},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state.Status != model.StatusGreen {
		t.Errorf("status = %v, want green", state.Status)
	}
	if state.SnapshotCount != 1 || state.TotalSize != 1000 || state.PackCount != 42 {
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

func TestRefreshStatsBestEffort(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic: fakeRestic{
			snaps:   []model.Snapshot{{Hostname: "homeserver", Time: now.Add(-time.Hour)}},
			statErr: errors.New("stats timed out"),
		},
	}
	state, err := a.Refresh(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Snapshots succeeded, so status is real; stats failure is only a partial error.
	if state.Status != model.StatusGreen {
		t.Errorf("status = %v, want green despite stats failure", state.Status)
	}
	if state.PartialErr == "" {
		t.Error("expected PartialErr from stats failure")
	}
}

func TestRefreshAll(t *testing.T) {
	cfg := testConfig()
	cfg.Repos = append(cfg.Repos,
		config.Repo{Name: "repo-b", Credential: "cred-a", Bucket: "bucket-b", ExpectedFrequency: config.Duration(24 * time.Hour)},
		config.Repo{Name: "repo-c", Credential: "cred-a", Bucket: "bucket-c", ExpectedFrequency: config.Duration(24 * time.Hour)},
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
