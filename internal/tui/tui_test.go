package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/cache"
	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
)

// --- fakes built on app's exported consumer-side interfaces ---

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type stubCache struct{ states map[string]model.RepoState }

func (s stubCache) Load(_ context.Context, name string) (model.RepoState, error) {
	if st, ok := s.states[name]; ok {
		return st, nil
	}
	return model.RepoState{}, cache.ErrMiss
}

func (s stubCache) Save(_ context.Context, name string, st model.RepoState) error {
	s.states[name] = st
	return nil
}

type stubSecrets struct{}

func (stubSecrets) Resolve(_, _ string) (secrets.Material, error) {
	return secrets.Material{AccessKey: "AK", SecretKey: "SK", ResticPassword: "pw"}, nil
}

type stubRestic struct{ snaps []model.Snapshot }

func (s stubRestic) Snapshots(_ context.Context, _ resticx.Target, _ resticx.Creds) ([]model.Snapshot, error) {
	return s.snaps, nil
}

func (stubRestic) Stats(_ context.Context, _ resticx.Target, _ resticx.Creds) (model.Stats, error) {
	return model.Stats{}, nil
}

// blockingRestic stalls in Snapshots until its context is cancelled, modeling a
// restic call still running when the user quits. It closes started once so a
// test can wait until the refresh has actually reached restic.
type blockingRestic struct{ started chan struct{} }

func (b blockingRestic) Snapshots(ctx context.Context, _ resticx.Target, _ resticx.Creds) ([]model.Snapshot, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingRestic) Stats(_ context.Context, _ resticx.Target, _ resticx.Creds) (model.Stats, error) {
	return model.Stats{}, nil
}

var testNow = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

func testApp(states map[string]model.RepoState) *app.App {
	if states == nil {
		states = map[string]model.RepoState{}
	}
	cfg := &config.Config{
		Global:      config.Global{Parallelism: 2},
		Credentials: []config.Credential{{Name: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto"}},
		Repos: []config.Repo{
			{Name: "repo-a", Credential: "cred-a", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour),
				Labels: map[string]string{"env": "home", "criticality": "high"}},
			{Name: "repo-b", Credential: "cred-a", Bucket: "b2", ExpectedFrequency: config.Duration(24 * time.Hour)},
		},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	return &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: states},
		Clock:   fixedClock{testNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{snaps: []model.Snapshot{{Hostname: "h", Time: testNow.Add(-time.Hour)}}},
	}
}

func newTestModel(t *testing.T, a *app.App) Model {
	t.Helper()
	rows, err := a.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newModel(ctx, cancel, a, rows, "0.18.1")
}

// press builds a KeyPressMsg for a single printable key. Key.String() returns
// Text when set, which is what key.Matches compares against.
func press(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: s, Code: []rune(s)[0]}
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

// --- tests ---

func TestViewRendersReposGlyphsAndMeta(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-2 * time.Hour), TotalSize: 442000000000, SnapshotCount: 240},
	})
	m := newTestModel(t, a)
	view := m.View().Content

	for _, want := range []string{
		"resticscope", "restic 0.18.1",
		"repo-a", "repo-b",
		statusGlyph(model.StatusGreen), // repo-a is green
		statusGlyph(model.StatusGrey),  // repo-b never refreshed
		"412 GiB", "240",
		"never refreshed",
		"fsn1 · high · home", // region + labels sorted by key (criticality, env)
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n---\n%s", want, view)
		}
	}
}

func TestCursorNavigationClamps(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if m.cursor != 0 {
		t.Fatalf("cursor starts at %d, want 0", m.cursor)
	}
	m = update(t, m, press("j"))
	if m.cursor != 1 {
		t.Errorf("after down, cursor = %d, want 1", m.cursor)
	}
	m = update(t, m, press("j")) // only two repos; must clamp
	if m.cursor != 1 {
		t.Errorf("down past end, cursor = %d, want 1 (clamped)", m.cursor)
	}
	m = update(t, m, press("k"))
	m = update(t, m, press("k")) // clamp at top
	if m.cursor != 0 {
		t.Errorf("up past start, cursor = %d, want 0 (clamped)", m.cursor)
	}
}

func TestQuit(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	next, cmd := m.Update(press("q"))
	nm := next.(Model)
	if !nm.quitting {
		t.Error("expected quitting = true")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("command did not produce tea.QuitMsg")
	}
	if nm.View().Content != "" {
		t.Errorf("expected empty view while quitting, got %q", nm.View().Content)
	}
}

func TestRefreshMarksPendingAndIgnoresDouble(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	next, cmd := m.Update(press("r"))
	nm := next.(Model)
	name := nm.rows[nm.cursor].Name
	if !nm.pending[name] {
		t.Errorf("%q not marked pending", name)
	}
	if cmd == nil {
		t.Fatal("expected a refresh command")
	}
	if _, cmd2 := nm.Update(press("r")); cmd2 != nil {
		t.Error("second refresh while pending should emit no command")
	}
}

func TestRefreshAllMarksEveryRepo(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	next, cmd := m.Update(press("R"))
	nm := next.(Model)
	for _, r := range nm.rows {
		if !nm.pending[r.Name] {
			t.Errorf("%q not pending after refresh-all", r.Name)
		}
	}
	if cmd == nil {
		t.Error("expected a batch command")
	}
}

func TestApplyRefreshUpdatesRowAndClearsPending(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m.pending["repo-a"] = true
	row := app.RepoStatus{
		Name:   "repo-a",
		Status: model.StatusGreen,
		State:  model.RepoState{Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 5},
	}
	nm := update(t, m, repoRefreshedMsg{name: "repo-a", row: row})
	if nm.pending["repo-a"] {
		t.Error("pending not cleared after refresh")
	}
	if got := findRow(nm, "repo-a"); got.Status != model.StatusGreen || got.State.SnapshotCount != 5 {
		t.Errorf("row not updated to live result: %+v", got)
	}
}

func TestApplyRefreshSurfacesSaveError(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	nm := update(t, m, repoRefreshedMsg{
		name: "repo-a",
		row:  app.RepoStatus{Name: "repo-a", Status: model.StatusGreen},
		err:  errors.New(`save cache for "repo-a": disk full`),
	})
	if !strings.Contains(nm.statusMsg, "cache write failed") {
		t.Errorf("statusMsg = %q, want a cache-write warning", nm.statusMsg)
	}
}

// The refresh command must drive the real app.RefreshRow path (semaphore,
// secrets, restic) and report back a repoRefreshedMsg with a live status.
func TestRefreshCommandRunsLiveRefresh(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	_, cmd := m.Update(press("r"))
	if cmd == nil {
		t.Fatal("expected a refresh command")
	}
	msg, ok := cmd().(repoRefreshedMsg)
	if !ok {
		t.Fatalf("command produced %T, want repoRefreshedMsg", cmd())
	}
	if msg.err != nil {
		t.Errorf("unexpected refresh error: %v", msg.err)
	}
	if msg.row.Status != model.StatusGreen { // stub restic returns a 1h-old snapshot
		t.Errorf("status = %v, want green", msg.row.Status)
	}
}

// Pressing q while a refresh is in flight must cancel the refresh context so the
// restic subprocess does not outlive the UI (Rule 10). The refresh command runs
// the real RefreshRow path against a restic that blocks until cancelled.
func TestQuitCancelsInFlightRefresh(t *testing.T) {
	a := testApp(nil)
	started := make(chan struct{})
	a.Restic = blockingRestic{started: started}
	m := newTestModel(t, a)

	_, cmd := m.Update(press("r"))
	if cmd == nil {
		t.Fatal("expected a refresh command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached restic")
	}

	m.Update(press("q")) // quits and cancels the shared refresh context

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not unblock after quit cancelled the context")
	}
}

// With refresh_on_open enabled, opening the TUI must schedule a refresh for cold
// rows. Both repos here have no cache, so both should be pending on open.
func TestRefreshOnOpenSchedulesColdRepos(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.RefreshOnOpen = true
	m := newTestModel(t, a)
	for _, r := range m.rows {
		if !m.pending[r.Name] {
			t.Errorf("%q not marked pending on open when refresh_on_open is set", r.Name)
		}
	}
}

func TestHelpToggle(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if m.help.ShowAll {
		t.Fatal("help should start collapsed")
	}
	if nm := update(t, m, press("?")); !nm.help.ShowAll {
		t.Error("? should expand the help view")
	}
}

func findRow(m Model, name string) app.RepoStatus {
	for _, r := range m.rows {
		if r.Name == name {
			return r
		}
	}
	return app.RepoStatus{}
}
