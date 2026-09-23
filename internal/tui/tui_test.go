package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/cache"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
	"github.com/alexzeitgeist/resticscope/internal/secrets"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

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
	return secrets.Material{Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}, ResticPassword: "pw"}, nil
}

type stubRestic struct {
	snaps       []model.Snapshot
	browseNodes []model.BrowseNode         // streamed by StreamSnapshotTree
	browseErr   error                      // returned after streaming (e.g. a restic failure)
	browseDelay time.Duration              // optional per-node delay to model a slow crawl
	findResults []model.FindSnapshotResult // returned by FindMatches
	findErr     error                      // optional error from FindMatches
	findCap     *stubFindCapture           // captures FindMatches args (host, pattern, calls)

	diffEntries     []model.DiffEntry // streamed by StreamDiff
	diffParseErrors int               // surfaced in the returned SnapshotDiff
	diffErr         error             // returned after streaming (e.g. a restic failure)
	diffCap         *stubDiffCapture  // captures StreamDiff args (older/newer, calls)

	treeNodes map[string]model.TreeNode // returned by TreeNode, keyed by treeKey
	treeErr   error                     // returned by TreeNode instead of a record
	treeCap   *stubTreeCapture          // captures TreeNode lookups across goroutines
}

// treeKey addresses one path's record in one snapshot for stubRestic.treeNodes.
func treeKey(snapshotID, p string) string { return snapshotID + ":" + p }

// stubTreeCapture records TreeNode lookups, which DiffNodes runs concurrently.
type stubTreeCapture struct {
	mu      sync.Mutex
	lookups []string // treeKey of each lookup
}

func (c *stubTreeCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.lookups)
}

// stubDiffCapture records the args passed to StreamDiff across goroutines so a
// test can assert the chronological older/newer ordering without a race.
type stubDiffCapture struct {
	mu       sync.Mutex
	calls    int
	olderID  string
	newerID  string
	metadata bool
}

func (c *stubDiffCapture) lastMetadata() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.metadata
}

func (c *stubDiffCapture) snapshot() (calls int, olderID, newerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.olderID, c.newerID
}

// stubFindCapture records the args passed to FindMatches across goroutines so
// a test can assert the host filter, pattern, and call count without a race.
type stubFindCapture struct {
	mu      sync.Mutex
	calls   int
	host    string
	pattern string
}

func (c *stubFindCapture) snapshot() (calls int, host, pattern string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.host, c.pattern
}

func (s stubRestic) Snapshots(_ context.Context, _ resticx.Target, _ resticx.Creds) ([]model.Snapshot, error) {
	return s.snaps, nil
}

func (stubRestic) CatConfig(_ context.Context, _ resticx.Target, _ resticx.Creds) error {
	return nil
}

// StreamSnapshotTree replays browseNodes through onNode, honoring context
// cancellation between nodes so a delayed crawl can be cancelled. A non-nil
// browseErr models a restic failure; otherwise it reports a complete scan.
func (s stubRestic) StreamSnapshotTree(ctx context.Context, _ resticx.Target, _ resticx.Creds, _ string, _ time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	for _, n := range s.browseNodes {
		if err := onNode(n); err != nil {
			return model.BrowseScanSummary{}, err
		}
		if s.browseDelay > 0 {
			select {
			case <-ctx.Done():
				return model.BrowseScanSummary{}, ctx.Err()
			case <-time.After(s.browseDelay):
			}
		}
	}
	if s.browseErr != nil {
		return model.BrowseScanSummary{}, s.browseErr
	}
	return model.BrowseScanSummary{Entries: len(s.browseNodes), IsComplete: true}, nil
}

// FindMatches returns the canned findResults / findErr, recording the host and
// pattern through findCap so a test can prove the host filter was wired through.
func (s stubRestic) FindMatches(_ context.Context, _ resticx.Target, _ resticx.Creds, host, pattern string) ([]model.FindSnapshotResult, error) {
	if s.findCap != nil {
		s.findCap.mu.Lock()
		s.findCap.calls++
		s.findCap.host = host
		s.findCap.pattern = pattern
		s.findCap.mu.Unlock()
	}
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.findResults, nil
}

// StreamDiff replays diffEntries through onEntry, then returns a SnapshotDiff
// carrying parseErrors and (if set) diffErr. diffCap records the older/newer
// argument order so a test can prove the chronological sort happens before the
// restic call. Most tui tests don't exercise diff at all; the zero stubRestic
// returns a zero SnapshotDiff without emitting any entries.
func (s stubRestic) StreamDiff(_ context.Context, _ resticx.Target, _ resticx.Creds, olderID, newerID string, metadata bool, _ time.Duration, onEntry func(model.DiffEntry) error, _ func(seen int)) (model.SnapshotDiff, error) {
	if s.diffCap != nil {
		s.diffCap.mu.Lock()
		s.diffCap.calls++
		s.diffCap.olderID = olderID
		s.diffCap.newerID = newerID
		s.diffCap.metadata = metadata
		s.diffCap.mu.Unlock()
	}
	for _, e := range s.diffEntries {
		if onEntry != nil {
			if err := onEntry(e); err != nil {
				return model.SnapshotDiff{}, err
			}
		}
	}
	if s.diffErr != nil {
		return model.SnapshotDiff{}, s.diffErr
	}
	return model.SnapshotDiff{ParseErrors: s.diffParseErrors}, nil
}

// TreeNode returns the treeNodes record for the looked-up path, or treeErr.
func (s stubRestic) TreeNode(_ context.Context, _ resticx.Target, _ resticx.Creds, snapshotID, dir, name string, _ time.Duration) (model.TreeNode, bool, error) {
	key := treeKey(snapshotID, path.Join(dir, name))
	if s.treeCap != nil {
		s.treeCap.mu.Lock()
		s.treeCap.lookups = append(s.treeCap.lookups, key)
		s.treeCap.mu.Unlock()
	}
	if s.treeErr != nil {
		return model.TreeNode{}, false, s.treeErr
	}
	n, ok := s.treeNodes[key]
	return n, ok, nil
}

// ExtractTree satisfies app.Restic; the tui tests don't exercise the extract
// orchestrator directly, so the stub is a no-op success.
func (s stubRestic) ExtractTree(_ context.Context, _ resticx.Target, _ resticx.Creds, _ resticx.ExtractTreeParams, _ func(resticx.ExtractTreeEvent) error) error {
	return nil
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

func (blockingRestic) CatConfig(_ context.Context, _ resticx.Target, _ resticx.Creds) error {
	return nil
}

// StreamSnapshotTree blocks until cancelled too, so an in-flight index can be
// used to test that quit/back cancel the running crawl just like a refresh.
func (b blockingRestic) StreamSnapshotTree(ctx context.Context, _ resticx.Target, _ resticx.Creds, _ string, _ time.Duration, _ func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	close(b.started)
	<-ctx.Done()
	return model.BrowseScanSummary{}, ctx.Err()
}

// FindMatches is unused by the blocking flows; defined to satisfy app.Restic.
func (blockingRestic) FindMatches(_ context.Context, _ resticx.Target, _ resticx.Creds, _, _ string) ([]model.FindSnapshotResult, error) {
	return nil, nil
}

// StreamDiff blocks until cancelled, mirroring Snapshots/StreamSnapshotTree, so
// a test can prove that q/esc while a diff is streaming cancels the running
// restic call.
func (b blockingRestic) StreamDiff(ctx context.Context, _ resticx.Target, _ resticx.Creds, _, _ string, _ bool, _ time.Duration, _ func(model.DiffEntry) error, _ func(seen int)) (model.SnapshotDiff, error) {
	close(b.started)
	<-ctx.Done()
	return model.SnapshotDiff{}, ctx.Err()
}

// TreeNode blocks until cancelled. It leaves started alone because DiffNodes
// runs two lookups at once.
func (blockingRestic) TreeNode(ctx context.Context, _ resticx.Target, _ resticx.Creds, _, _, _ string, _ time.Duration) (model.TreeNode, bool, error) {
	<-ctx.Done()
	return model.TreeNode{}, false, ctx.Err()
}

// ExtractTree blocks until cancelled, mirroring the other blocking flows, so a
// test could prove q/esc cancels a running extract.
func (b blockingRestic) ExtractTree(ctx context.Context, _ resticx.Target, _ resticx.Creds, _ resticx.ExtractTreeParams, _ func(resticx.ExtractTreeEvent) error) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

var testNow = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

func testApp(states map[string]model.RepoState) *app.App {
	if states == nil {
		states = map[string]model.RepoState{}
	}
	cfg := &config.Config{
		Global: config.Global{Parallelism: 2},
		Repos: []config.Repo{
			{
				Name: "repo-a", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour),
				Labels: map[string]string{"env": "home", "criticality": "high"},
			},
			{Name: "repo-b", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b2", ExpectedFrequency: config.Duration(24 * time.Hour)},
		},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	// Use an untagged non-interactive shell so discarded sessions create no temp
	// prompt files and tests remain independent of the developer's SHELL.
	cfg.Global.Shell = "/bin/sh"
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
	rows, err := a.Statuses(t.Context())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
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

// leafCmds flattens a possibly-batched command into its leaf commands. The
// refresh keys now batch the spinner tick with the live refresh, so a test that
// wants the refresh command unwraps the batch first. Running the batch wrapper
// only assembles the slice; it never runs the leaves, so this is safe even
// when a leaf (the refresh) would block.
func leafCmds(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	if cmd == nil {
		return nil
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		return batch
	}
	return []tea.Cmd{cmd}
}

func TestViewRendersReposGlyphsAndMeta(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-2 * time.Hour), SnapshotCount: 240},
	})
	m := newTestModel(t, a)
	view := m.View().Content

	for _, want := range []string{
		"resticscope", "restic 0.18.1",
		"Name", "Last", "Snaps", "Labels", // dim table header
		"repo-a", "repo-b",
		statusGlyph(model.StatusGreen), // repo-a is green
		statusGlyph(model.StatusGrey),  // repo-b never refreshed
		"240",
		"never refreshed",
		"high · home", // labels column, sorted by key (criticality, env); region no longer rendered
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n---\n%s", want, view)
		}
	}
	// Region moved out of the visible columns; the Labels-column value must not
	// embed it the way the old sub-line did.
	if strings.Contains(view, "fsn1 ·") {
		t.Errorf("view should not embed region in the labels column\n---\n%s", view)
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

// Page-down/up jump a whole window and clamp at the ends. The default test app
// has two repos in a tall pane, so one page spans the entire list: page-down
// lands on the last repo, page-up returns to the first.
func TestListPageNavigationClamps(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.cursor != 1 {
		t.Errorf("page-down cursor = %d, want 1 (clamped to last)", m.cursor)
	}
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown}) // already at end
	if m.cursor != 1 {
		t.Errorf("page-down past end cursor = %d, want 1", m.cursor)
	}
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.cursor != 0 {
		t.Errorf("page-up cursor = %d, want 0 (clamped to first)", m.cursor)
	}
}

func TestDetailPageNavigationClamps(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.snapCursor != 2 {
		t.Errorf("page-down snapCursor = %d, want 2 (clamped to last)", m.snapCursor)
	}
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.snapCursor != 0 {
		t.Errorf("page-up snapCursor = %d, want 0 (clamped to first)", m.snapCursor)
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

func TestRunQuitReturnsNil(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	if err := runProgram(ctx, testApp(nil), "0.18.1",
		tea.WithInput(bytes.NewBufferString("q")),
		tea.WithOutput(io.Discard),
	); err != nil {
		t.Fatalf("runProgram returned error on normal quit: %v", err)
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

// The spinner must stop ticking once nothing is refreshing: an idle spinner is
// invisible, so re-arming its tick only re-renders a hidden frame ~12x/second
// and burns CPU forever (this was the cause of ~5% idle CPU). While a repo is
// pending the tick must keep going so the glyph animates.
func TestSpinnerTickStopsWhenIdle(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if _, cmd := m.Update(spinner.TickMsg{}); cmd != nil {
		t.Error("spinner kept ticking while idle; it should stop to spare CPU")
	}
	m.pending["repo-a"] = true
	if _, cmd := m.Update(spinner.TickMsg{}); cmd == nil {
		t.Error("spinner stopped ticking while a repo was refreshing")
	}
}

// Starting a refresh from idle must restart the spinner tick loop, since the
// loop stops itself once pending drains; otherwise the glyph would never spin.
func TestRefreshFromIdleRestartsSpinner(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	_, cmd := m.Update(press("r"))
	var sawTick bool
	for _, c := range leafCmds(t, cmd) {
		if _, ok := c().(spinner.TickMsg); ok {
			sawTick = true
		}
	}
	if !sawTick {
		t.Error("pressing r from idle did not (re)start the spinner tick")
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
	// Pressing r also starts the spinner, so the refresh arrives inside a batch;
	// run each leaf and keep the live refresh result.
	var msg repoRefreshedMsg
	var found bool
	for _, c := range leafCmds(t, cmd) {
		if rm, ok := c().(repoRefreshedMsg); ok {
			msg, found = rm, true
		}
	}
	if !found {
		t.Fatal("refresh command produced no repoRefreshedMsg")
	}
	if msg.err != nil {
		t.Errorf("unexpected refresh error: %v", msg.err)
	}
	if msg.row.Status != model.StatusGreen { // stub restic returns a 1h-old snapshot
		t.Errorf("status = %v, want green", msg.row.Status)
	}
}

// Pressing q while a refresh is in flight must cancel the refresh context so the
// restic subprocess does not outlive the UI. The refresh command runs
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
	// r batches the spinner tick with the refresh; run every leaf (the tick
	// returns at once, only the refresh blocks) and forward just the refresh
	// result so the cancel assertion below isn't satisfied by the tick.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		go func() {
			if msg, ok := c().(repoRefreshedMsg); ok {
				done <- msg
			}
		}()
	}

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

// `?` opens the full-screen help overlay, which lists the keybindings grouped by
// context plus a status-glyph legend; `?` again closes it back to the list. The
// terminal here is large enough for the full two-column layout so every section
// is on screen at once; the narrow/short cases are covered separately below.
func TestHelpOverlayToggle(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if m.view != listView {
		t.Fatalf("view starts at %d, want listView", m.view)
	}
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 60})
	m = update(t, m, press("?"))
	if m.view != helpView {
		t.Fatalf("? should open the help overlay, view = %d", m.view)
	}
	view := m.View().Content
	for _, want := range []string{
		"keybindings",                                  // overlay title
		"Global", "List", "Detail", "Filter", "Status", // section headings
		"refresh all repos", // the genuinely-global action
		"move repo cursor",  // cursor movement lives under List, not Global
		"page up/down",      // page navigation in the lists
		"shell at snapshot", // a detail-only action
		"swap snapshot direction",
		"never refreshed", // glyph legend entry
	} {
		if !strings.Contains(view, want) {
			t.Errorf("help overlay missing %q\n---\n%s", want, view)
		}
	}
	m = update(t, m, press("?"))
	if m.view != listView {
		t.Errorf("? should close the overlay back to the list, view = %d", m.view)
	}
}

// On a terminal wide enough for both columns the overlay keeps the side-by-side
// layout (a left-column heading and a right-column heading share a line), and
// when the body also fits vertically the footer must not advertise scroll keys,
// the same minimal-footer rule the info modal locks in.
func TestHelpOverlayTwoColumnsWhenWide(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 60})
	m = update(t, m, press("?"))
	view := stripANSI(m.View().Content)
	var sideBySide bool
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, "Global") && strings.Contains(line, "Detail") {
			sideBySide = true
		}
	}
	if !sideBySide {
		t.Errorf("at 140 cols Global and Detail should share a line (two columns)\n---\n%s", view)
	}
	if strings.Contains(view, "showing lines") {
		t.Errorf("body fits at 140x60, no scroll window expected\n---\n%s", view)
	}
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if footer := lines[len(lines)-1]; strings.Contains(footer, "scroll") {
		t.Errorf("help footer should not advertise scroll keys when the body fits\nfooter: %q", footer)
	}
}

// Below the two-column width the overlay stacks into a single column: every
// rendered line stays inside the terminal width and the full reference
// (including the sections that used to live in the truncated right column)
// survives in the scrollable line list instead of being cut off.
func TestHelpOverlayNarrowStacksSingleColumn(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 30})
	m = update(t, m, press("?"))
	view := stripANSI(m.View().Content)
	for line := range strings.SplitSeq(view, "\n") {
		if got := lipgloss.Width(line); got > 80 {
			t.Errorf("line wider than terminal: %d > 80: %q", got, line)
		}
	}
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, "Global") && strings.Contains(line, "Detail") {
			t.Errorf("at 80 cols the columns must stack, but found a side-by-side line: %q", line)
		}
	}
	// The stacked column is taller than the pane, so the body must window
	// itself and advertise the position.
	if !strings.Contains(view, "showing lines") {
		t.Errorf("stacked overlay should report its scroll window\n---\n%s", view)
	}
	// Nothing is lost to the narrower layout: the deep sections are still in
	// the line list the window scrolls over.
	all := stripANSI(strings.Join(m.helpBodyLines(80), "\n"))
	for _, want := range []string{
		"swap snapshot direction",        // Diff section
		"file versions across snapshots", // Browse section, truncated pre-fix
		"never refreshed",                // glyph legend
		"extract whole snapshot",         // Detail section
	} {
		if !strings.Contains(all, want) {
			t.Errorf("single-column help lost %q", want)
		}
	}
	// On a pane too narrow even for one column, lines clip instead of wrapping
	// (wrapped lines would break the fixed row budget the frame relies on).
	for _, line := range m.helpBodyLines(40) {
		if got := lipgloss.Width(line); got > 40 {
			t.Errorf("line wider than 40-col pane: %d: %q", got, line)
		}
	}
}

// A help body taller than the terminal must remain reachable via up/down/page
// navigation, and reopening the overlay starts back at the top.
func TestHelpOverlayScrolls(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 30})
	m = update(t, m, press("?"))
	if m.helpScroll != 0 {
		t.Fatalf("fresh overlay helpScroll = %d, want 0", m.helpScroll)
	}
	initial := stripANSI(m.View().Content)
	if strings.Contains(initial, "never refreshed") {
		t.Fatalf("precondition: the glyph legend must start below the fold\n---\n%s", initial)
	}
	if !strings.Contains(initial, "scroll") {
		t.Errorf("help footer missing the scroll chip while scrolling is needed\n---\n%s", initial)
	}

	// Page-down enough times to reach the bottom; clampModalScroll bounds the
	// stored offset, so excess presses are a no-op once the floor is hit.
	for range 20 {
		m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	bottom := stripANSI(m.View().Content)
	if !strings.Contains(bottom, "never refreshed") {
		t.Errorf("after scrolling to the bottom, the glyph legend is still hidden\n---\n%s", bottom)
	}

	// j/k must also scroll (they share keys.Up/Down bindings; no list cursor
	// exists behind the modal to claim them).
	for range 20 {
		m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	m = update(t, m, press("j"))
	if m.helpScroll != 1 {
		t.Errorf("after j, helpScroll = %d, want 1", m.helpScroll)
	}
	m = update(t, m, press("k"))
	if m.helpScroll != 0 {
		t.Errorf("after k, helpScroll = %d, want 0", m.helpScroll)
	}

	// Close scrolled-down, reopen: the overlay starts at the top again.
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = update(t, m, press("?"))
	m = update(t, m, press("?"))
	if m.helpScroll != 0 {
		t.Errorf("reopened overlay helpScroll = %d, want 0", m.helpScroll)
	}
}

func TestHelpHeaderClipsNarrowTerminal(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m.width = 12
	header := m.titleRow(m.helpTitle())
	if got := lipgloss.Width(header); got > m.width {
		t.Fatalf("help header width = %d, want <= %d: %q", got, m.width, header)
	}
}

// The overlay returns to the view it was opened from, and `esc` closes it too.
func TestHelpOverlayReturnsToOrigin(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter")) // open detail
	if m.view != detailView {
		t.Fatalf("expected detail view, got %d", m.view)
	}
	m = update(t, m, press("?"))
	if m.view != helpView {
		t.Fatalf("? should open the overlay, got %d", m.view)
	}
	m = update(t, m, press("esc"))
	if m.view != detailView {
		t.Errorf("esc from help should return to the detail view, got %d", m.view)
	}
}

// The overlay is modal: action keys behind it (here refresh-all) do nothing.
func TestHelpOverlayIsModal(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("?"))
	next, cmd := m.Update(press("R"))
	nm := next.(Model)
	if len(nm.pending) != 0 {
		t.Errorf("refresh-all should not run behind the help overlay: %v", nm.pending)
	}
	if cmd != nil {
		t.Error("R behind the help overlay should emit no command")
	}
	if nm.view != helpView {
		t.Errorf("R should not leave the help overlay, view = %d", nm.view)
	}
}

// keyLabel renders a binding's keys with arrow/symbol substitutions and all its
// alternates, so the overlay can't drift from the keys the handlers match.
func TestKeyLabel(t *testing.T) {
	k := defaultKeys()
	for _, tc := range []struct {
		b    key.Binding
		want string
	}{
		{k.Up, "↑/k"},
		{k.Down, "↓/j"},
		{k.Back, "esc"},        // back dropped b; q is matched separately
		{k.Quit, "q"},          // context-aware quit/back is bare q
		{k.HardQuit, "ctrl+c"}, // unconditional hard quit
		{k.Enter, "enter"},
		{k.FilterDelete, "⌫"},
	} {
		if got := keyLabel(tc.b); got != tc.want {
			t.Errorf("keyLabel(%v) = %q, want %q", tc.b.Keys(), got, tc.want)
		}
	}
}

// Footer text distinguishes list quit from nested back, while the title owns
// the persistent help chip. Strip ANSI spans before matching key-label pairs.
func TestBackQuitFooterAndHeaderRendering(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})

	listFooter := stripANSI(m.footerView())
	if !strings.Contains(listFooter, "q quit") {
		t.Errorf("list footer should advertise 'q quit'\n---\n%s", listFooter)
	}
	if strings.Contains(listFooter, "b back") {
		t.Errorf("list footer should not mention the removed 'b back'\n---\n%s", listFooter)
	}

	m = update(t, m, press("enter"))
	header := stripANSI(m.titleRow(m.detailTitle()))
	if !strings.Contains(header, "detail: ") {
		t.Errorf("detail title should carry the 'detail: ' prefix\n---\n%s", header)
	}
	if !strings.Contains(header, "? help") {
		t.Errorf("detail title row should carry the persistent '? help' chip\n---\n%s", header)
	}
	if detailFooter := stripANSI(m.footerView()); !strings.Contains(detailFooter, "q back") {
		t.Errorf("detail footer should advertise 'q back'\n---\n%s", detailFooter)
	}
	if content := stripANSI(m.View().Content); strings.Contains(content, "b back") {
		t.Errorf("detail view should not contain the removed 'b back'\n---\n%s", content)
	}
	if content := stripANSI(m.View().Content); strings.Contains(content, "esc/q back") {
		t.Errorf("detail view should not advertise esc in the compact back hint\n---\n%s", content)
	}
	m = update(t, m, press("?"))
	if helpFooter := stripANSI(m.footerView()); !strings.Contains(helpFooter, "q back") {
		t.Errorf("help footer should advertise 'q back'\n---\n%s", helpFooter)
	}
	if helpFooter := stripANSI(m.footerView()); strings.Contains(helpFooter, "b back") {
		t.Errorf("help footer should not mention the removed 'b back'\n---\n%s", helpFooter)
	}
	if helpFooter := stripANSI(m.footerView()); strings.Contains(helpFooter, "esc/q back") {
		t.Errorf("help footer should not advertise esc in the compact back hint\n---\n%s", helpFooter)
	}
}

// The Enter key does something different in each view (open detail in the list,
// browse the selected snapshot in detail, open the directory in browse), so the
// footer label is overridden per view by enterAs. This guards against the binding
// reverting to a single generic label that would mislead in two views out of three.
func TestFooterEnterLabelsByView(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})

	listFooter := stripANSI(m.footerView())
	if !strings.Contains(listFooter, "enter detail") {
		t.Errorf("list footer should advertise 'enter detail'\n---\n%s", listFooter)
	}
	if strings.Contains(listFooter, "open/shell") {
		t.Errorf("list footer must not show the old 'open/shell' label\n---\n%s", listFooter)
	}

	m = update(t, m, press("enter")) // -> detail view
	detailFooter := stripANSI(m.footerView())
	if !strings.Contains(detailFooter, "enter browse") {
		t.Errorf("detail footer should advertise 'enter browse'\n---\n%s", detailFooter)
	}
	if strings.Contains(detailFooter, "enter shell") {
		t.Errorf("detail footer must not show the old 'enter shell' label\n---\n%s", detailFooter)
	}
	if strings.Contains(detailFooter, "open/shell") {
		t.Errorf("detail footer must not show the old 'open/shell' label\n---\n%s", detailFooter)
	}

	// Browse needs its own fixture (a populated browse store); reuse openBrowse.
	bm := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))
	bm = update(t, bm, tea.WindowSizeMsg{Width: 200, Height: 40})
	browseFooter := stripANSI(bm.footerView())
	if !strings.Contains(browseFooter, "enter open") {
		t.Errorf("browse footer should advertise 'enter open'\n---\n%s", browseFooter)
	}
	if strings.Contains(browseFooter, "open/shell") {
		t.Errorf("browse footer must not show the old 'open/shell' label\n---\n%s", browseFooter)
	}
}

// stripANSI removes SGR color/style escape sequences so tests can match the
// underlying text regardless of the terminal color profile under which the
// styled output was rendered.
func stripANSI(s string) string {
	return ansiSGR.ReplaceAllString(s, "")
}

var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// The full-screen help overlay must reflect the context-aware scheme: q is no
// longer a global quit key (it lives under List, where it actually exits), the
// only unconditional quit advertised globally is ctrl+c, and the detail back row
// documents both back keys (esc/q) derived from the live Back and Quit bindings.
func TestHelpOverlayDescribesContextAwareQuit(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	left, right := m.helpColumns()

	find := func(secs []helpSection, title string) (helpSection, bool) {
		for _, s := range secs {
			if s.title == title {
				return s, true
			}
		}
		return helpSection{}, false
	}
	hasEntry := func(s helpSection, keys, desc string) bool {
		for _, e := range s.entries {
			if e.keys == keys && e.desc == desc {
				return true
			}
		}
		return false
	}
	hasKeys := func(s helpSection, keys string) bool {
		for _, e := range s.entries {
			if e.keys == keys {
				return true
			}
		}
		return false
	}

	global, ok := find(left, "Global")
	if !ok {
		t.Fatal("help overlay missing Global section")
	}
	if !hasEntry(global, "ctrl+c", "quit") {
		t.Errorf("Global section should document 'ctrl+c quit', got %+v", global.entries)
	}
	if hasKeys(global, "q") {
		t.Errorf("q must not appear in the Global section (it is not a global quit), got %+v", global.entries)
	}

	list, ok := find(left, "List")
	if !ok {
		t.Fatal("help overlay missing List section")
	}
	if !hasEntry(list, "q", "quit") {
		t.Errorf("List section should document 'q quit', got %+v", list.entries)
	}

	detail, ok := find(right, "Detail")
	if !ok {
		t.Fatal("help overlay missing Detail section")
	}
	if !hasEntry(detail, "esc/q", "back to the list") {
		t.Errorf("Detail section should document 'esc/q back to the list', got %+v", detail.entries)
	}
}

// detailApp seeds repo-a with three snapshots and observed hosts/tags. It uses
// env password mode so building a shell session in tests never writes a temp
// password file to disk.
func detailApp(t *testing.T) *app.App {
	t.Helper()
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name:          "repo-a",
			RefreshedAt:   testNow,
			LastSnapshot:  testNow.Add(-time.Hour),
			SnapshotCount: 3,
			Hosts:         []string{"homeserver"},
			Tags:          []string{"daily"},
			Snapshots: []model.Snapshot{
				{ID: "id-oldest", ShortID: "s1", Time: testNow.Add(-3 * time.Hour), Hostname: "homeserver", Tags: []string{"daily"}},
				{ID: "id-middle", ShortID: "s2", Time: testNow.Add(-2 * time.Hour), Hostname: "homeserver", Tags: []string{"daily"}},
				{
					ID: "id-newest", ShortID: "s3", Time: testNow.Add(-1 * time.Hour), Hostname: "homeserver",
					Tags: []string{"daily"}, ProgramVersion: "restic 0.18.1",
					Summary: &model.SnapshotSummary{
						TotalBytesProcessed: 4404019200, DataAdded: new(int64(5242880)), DataAddedPacked: new(int64(4194304)),
						BackupStart: testNow.Add(-1 * time.Hour), BackupEnd: testNow.Add(-1*time.Hour + 28*time.Second),
						FilesNew: new(uint64(12)), FilesChanged: new(uint64(34)), TotalFilesProcessed: new(uint64(4096)),
					},
				},
			},
		},
	})
	a.Cfg.Global.ShellPasswordMode = "env"
	return a
}

func TestEnterOpensDetailView(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("view = %d, want detailView", m.view)
	}
	view := m.View().Content
	// The default test width (100) promotes both extra columns, so Added/Took
	// are table columns and the panel slims to non-columnar facts.
	for _, want := range []string{
		"repo-a",           // detail header
		"Repository",       // metadata block
		"Program",          // meta rollup of observed restic versions
		"restic 0.18.1",    // the observed version value
		"Snapshots",        // snapshot table heading
		"2026-05-23 13:00", // newest snapshot (testNow - 1h)
		"homeserver",
		"s3",             // short id, in the table and the sub-panel heading
		"Added",          // promoted column header
		"Took",           // promoted column header
		"+5.0 MiB",       // added bytes, now their own column value
		"28s",            // backup duration, now in the Took column
		"Selected",       // selected-snapshot sub-panel
		"4.0 MiB packed", // packed bytes, panel drops the duplicated "+X added"
		"id-newest",      // full id in the sub-panel
		"Backup",         // exact backup window in the selected-snapshot panel
		"2026-05-23 13:00:00 → 2026-05-23 13:00:28",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view missing %q\n---\n%s", want, view)
		}
	}
	// Added/Took are columns now, so the panel must not repeat them.
	for _, dup := range []string{"5.0 MiB added", "took 28s"} {
		if strings.Contains(view, dup) {
			t.Errorf("detail view duplicates columnar field %q in the panel\n---\n%s", dup, view)
		}
	}
	// "Versions" is the file-versions feature's word; restic's ProgramVersion
	// rollup is labeled "Program" (matching the info view).
	if strings.Contains(view, "Versions") {
		t.Errorf("detail view must not label the restic-version rollup 'Versions'\n---\n%s", view)
	}
}

func TestSnapshotDetailSurfacesUsernameAndBackupWindow(t *testing.T) {
	a := detailApp(t)
	cache := a.Cache.(stubCache)
	state := cache.states["repo-a"]
	state.Snapshots[2].Username = "backup-user" // id-newest, the cursor row
	cache.states["repo-a"] = state

	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	view := m.View().Content

	for _, want := range []string{
		"User",
		"backup-user",
		"Backup",
		"2026-05-23 13:00:00 → 2026-05-23 13:00:28",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("selected-snapshot panel missing %q\n---\n%s", want, view)
		}
	}
}

// A backup that crosses midnight (or spans days) must render the end timestamp
// with its own full date, not a bare clock time, so the completion date is never
// ambiguous.
func TestSnapshotDetailBackupWindowCrossesDate(t *testing.T) {
	start := time.Date(2026, 5, 23, 23, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 24, 0, 10, 0, 0, time.UTC)
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: start, SnapshotCount: 1,
			Snapshots: []model.Snapshot{
				{
					ID: "id-cross", ShortID: "x", Time: start, Hostname: "h",
					Summary: &model.SnapshotSummary{
						TotalBytesProcessed: 1024, BackupStart: start, BackupEnd: end,
					},
				},
			},
		},
	})

	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	view := m.View().Content
	if !strings.Contains(view, "2026-05-23 23:30:00 → 2026-05-24 00:10:00") {
		t.Errorf("selected-snapshot panel should show the end date on a midnight-crossing backup\n---\n%s", view)
	}
}

func TestSnapshotChurnOmitsMissingFields(t *testing.T) {
	got := snapshotChurn(&model.SnapshotSummary{DataAdded: new(int64(5242880))}, true)
	if got != "+5.0 MiB added" {
		t.Errorf("snapshotChurn with missing packed/file fields = %q, want only added bytes", got)
	}
	if strings.Contains(got, "0 B") || strings.Contains(got, "packed") {
		t.Errorf("snapshotChurn should not synthesize missing packed bytes: %q", got)
	}
	if got := snapshotChurn(&model.SnapshotSummary{}, true); got != "churn unavailable" {
		t.Errorf("snapshotChurn(empty summary) = %q, want churn unavailable", got)
	}
}

// When the Added column is present (includeAdded == false), the churn line drops
// the duplicated "+X added" piece and leads with bare "Y packed"; a summary that
// has nothing left to say after de-duplication collapses to an em-dash, while a
// wholly absent summary still reports "no summary".
func TestSnapshotChurnDropsAddedWhenColumnShown(t *testing.T) {
	sum := &model.SnapshotSummary{
		DataAdded: new(int64(5242880)), DataAddedPacked: new(int64(4194304)),
		FilesNew: new(uint64(12)), FilesChanged: new(uint64(34)), TotalFilesProcessed: new(uint64(4096)),
	}
	got := snapshotChurn(sum, false)
	if want := "4.0 MiB packed · 12 new · 34 changed · 4096 files"; got != want {
		t.Errorf("snapshotChurn(includeAdded=false) = %q, want %q", got, want)
	}
	if strings.Contains(got, "added") || strings.Contains(got, "(") {
		t.Errorf("snapshotChurn(includeAdded=false) should drop the added piece and its parens: %q", got)
	}
	// Only added bytes present: nothing remains once Added owns it.
	if got := snapshotChurn(&model.SnapshotSummary{DataAdded: new(int64(5242880))}, false); got != "—" {
		t.Errorf("snapshotChurn(includeAdded=false, added-only) = %q, want em-dash", got)
	}
	if got := snapshotChurn(nil, false); got != "no summary" {
		t.Errorf("snapshotChurn(nil) = %q, want no summary", got)
	}
}

func TestDetailSnapshotsUseModelOrdering(t *testing.T) {
	a := detailApp(t)
	cache := a.Cache.(stubCache)
	state := cache.states["repo-a"]
	tm := testNow.Add(-time.Hour)
	state.Snapshots = []model.Snapshot{
		{ID: "a", ShortID: "a", Time: tm, Summary: &model.SnapshotSummary{BackupStart: tm, BackupEnd: tm.Add(10 * time.Second)}},
		{ID: "b", ShortID: "b", Time: tm, Summary: &model.SnapshotSummary{BackupStart: tm, BackupEnd: tm.Add(20 * time.Second)}},
	}
	state.SnapshotCount = len(state.Snapshots)
	cache.states["repo-a"] = state

	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	// Narrow pane: no Took column, so the duration lives in the selected panel's
	// backup-window row, which is where the test proves the selected detail
	// tracks the ordered snapshot.
	m.width, m.height = 80, 40
	snaps := m.detailSnapshots()
	if len(snaps) != 2 || snaps[0].ID != "b" {
		t.Fatalf("detailSnapshots first = %+v, want ID b", snaps)
	}
	if snap := m.selectedSnapshot(); snap == nil || snap.ID != "b" {
		t.Fatalf("selectedSnapshot = %+v, want ID b", snap)
	}
	if !strings.Contains(m.View().Content, "2026-05-23 13:00:00 → 2026-05-23 13:00:20 (20s)") {
		t.Fatalf("detail view did not use the same ordered snapshot for selected detail:\n%s", m.View().Content)
	}
}

func TestDetailBackReturnsToList(t *testing.T) {
	for _, k := range []string{"esc", "q"} {
		m := newTestModel(t, detailApp(t))
		next, _ := m.Update(press("enter"))
		m = next.(Model)
		if m.view != detailView {
			t.Fatal("expected detail view after enter")
		}
		next, cmd := m.Update(press(k))
		m = next.(Model)
		if m.view != listView {
			t.Errorf("%q did not return to the list view", k)
		}
		// q steps back from the detail view rather than quitting, so it must not
		// set quitting or emit a quit command on its way home.
		if m.quitting {
			t.Errorf("%q from detail should not set quitting", k)
		}
		if cmd != nil {
			t.Errorf("%q from detail should emit no command", k)
		}
	}
}

// b (and enter, its drill-in alias on the detail view) opens the in-app file
// browser for the selected snapshot: it switches to browseView (showing the
// indexing state with no listing yet), marks the index in flight, and returns
// the command that runs the one-time index.
func TestDetailBKeyStartsBrowse(t *testing.T) {
	for _, k := range []string{"b", "enter"} {
		t.Run(k, func(t *testing.T) {
			m := newTestModel(t, browseApp(t))
			m = update(t, m, press("enter"))
			if m.view != detailView {
				t.Fatal("expected detail view after enter")
			}
			next, cmd := m.Update(press(k))
			nm := next.(Model)
			if nm.view != browseView {
				t.Errorf("%q should open the browse view; view = %d", k, nm.view)
			}
			if !nm.isBrowseLoading || nm.browseIndexed {
				t.Errorf("%q should mark the index in flight, not yet indexed: loading=%v indexed=%v", k, nm.isBrowseLoading, nm.browseIndexed)
			}
			if nm.browseRepo != "repo-a" || nm.browseSnapshot != "id-newest" {
				t.Errorf("browse target = %q/%q, want repo-a/id-newest", nm.browseRepo, nm.browseSnapshot)
			}
			if cmd == nil {
				t.Errorf("%q should emit a browse command", k)
			}
		})
	}
}

// q is context-aware: on a nested view it steps back instead of quitting. From
// the detail view it returns to the list without quitting; from the help overlay
// it closes back to the view that opened it. ctrl+c still hard-quits from a
// nested view.
func TestQuitKeyStepsBackFromNestedViews(t *testing.T) {
	t.Run("q on detail returns to list without quitting", func(t *testing.T) {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter"))
		next, cmd := m.Update(press("q"))
		nm := next.(Model)
		if nm.view != listView {
			t.Errorf("q from detail should return to the list, view = %d", nm.view)
		}
		if nm.quitting {
			t.Error("q from detail should not set quitting")
		}
		if cmd != nil {
			t.Error("q from detail should emit no quit command")
		}
	})
	t.Run("q on help closes the overlay to the previous view", func(t *testing.T) {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter")) // detail
		m = update(t, m, press("?"))     // open help over detail
		if m.view != helpView {
			t.Fatalf("expected help view, got %d", m.view)
		}
		next, cmd := m.Update(press("q"))
		nm := next.(Model)
		if nm.view != detailView {
			t.Errorf("q from help should return to the detail view, got %d", nm.view)
		}
		if nm.quitting {
			t.Error("q from help should not set quitting")
		}
		if cmd != nil {
			t.Error("q from help should emit no quit command")
		}
	})
	t.Run("ctrl+c hard-quits from a nested view", func(t *testing.T) {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter"))
		next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if !next.(Model).quitting {
			t.Error("ctrl+c should quit from the detail view")
		}
		if cmd == nil {
			t.Fatal("expected a quit command")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("ctrl+c did not produce tea.QuitMsg")
		}
	})
}

func TestQuitWithNilCancelDoesNotPanic(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	for _, tc := range []struct {
		name string
		m    Model
		msg  tea.KeyPressMsg
	}{
		{name: "global ctrl+c", m: Model{keys: defaultKeys()}, msg: ctrlC},
		{name: "list q", m: Model{keys: defaultKeys()}, msg: press("q")},
		{name: "filter ctrl+c", m: Model{keys: defaultKeys(), filtering: true}, msg: ctrlC},
		{name: "browse search ctrl+c", m: Model{keys: defaultKeys(), browseSearching: true}, msg: ctrlC},
		{name: "diff search ctrl+c", m: Model{keys: defaultKeys(), diffSearching: true}, msg: ctrlC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, cmd := tc.m.Update(tc.msg)
			if !next.(Model).quitting {
				t.Error("quit key should set quitting even when cancel is nil")
			}
			if cmd == nil {
				t.Fatal("expected a quit command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Errorf("quit key did not produce tea.QuitMsg")
			}
		})
	}
}

func TestDetailSnapshotCursorNavigatesAndClamps(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.snapCursor != 0 {
		t.Fatalf("snapCursor starts at %d, want 0", m.snapCursor)
	}
	m = update(t, m, press("j"))
	if m.snapCursor != 1 {
		t.Errorf("after down, snapCursor = %d, want 1", m.snapCursor)
	}
	m = update(t, m, press("j"))
	m = update(t, m, press("j")) // only three snapshots; must clamp at 2
	if m.snapCursor != 2 {
		t.Errorf("snapCursor = %d, want 2 (clamped)", m.snapCursor)
	}
	// The selected snapshot tracks the cursor in newest-first order.
	if snap := m.selectedSnapshot(); snap == nil || snap.ID != "id-oldest" {
		t.Errorf("selectedSnapshot = %+v, want the oldest", snap)
	}
}

func TestShellKeyReturnsCommand(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	_, cmd := m.Update(press("s"))
	if cmd == nil {
		t.Fatal("pressing s should produce a shell command")
	}
}

// When the session cannot be prepared (e.g. secrets fail), the shell key must
// surface the failure in the footer rather than crash or silently no-op.
func TestShellPrepFailureSurfacesError(t *testing.T) {
	a := detailApp(t)
	a.Secrets = failingSecrets{}
	m := newTestModel(t, a)

	_, cmd := m.Update(press("s"))
	if cmd == nil {
		t.Fatal("expected a command even on prep failure")
	}
	msg, ok := cmd().(shellExitedMsg)
	if !ok {
		t.Fatalf("command produced %T, want shellExitedMsg", cmd())
	}
	if msg.err == nil {
		t.Fatal("expected a preparation error")
	}
	nm := update(t, m, msg)
	if !strings.Contains(nm.statusMsg, "shell:") {
		t.Errorf("statusMsg = %q, want a shell error notice", nm.statusMsg)
	}
}

type failingSecrets struct{}

func (failingSecrets) Resolve(_, _ string) (secrets.Material, error) {
	return secrets.Material{}, errors.New("secrets: no repo \"repo-a\"")
}

func findRow(m Model, name string) app.RepoStatus {
	for _, r := range m.rows {
		if r.Name == name {
			return r
		}
	}
	return app.RepoStatus{}
}

func names(rows []app.RepoStatus) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func visNames(m Model) string { return strings.Join(names(m.displayList().rows), ",") }

func typeFilter(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = update(t, m, press(string(r)))
	}
	return m
}

func TestSortRowsOrders(t *testing.T) {
	src := []app.RepoStatus{
		{Name: "Grey", Status: model.StatusGrey},
		{Name: "Amber", Status: model.StatusAmber},
		{Name: "Echo", Status: model.StatusError},
		{Name: "green", Status: model.StatusGreen},
		{Name: "red-1", Status: model.StatusRed},
	}
	clone := func() []app.RepoStatus { return append([]app.RepoStatus(nil), src...) }

	for _, tc := range []struct {
		mode sortMode
		want string
	}{
		{sortConfig, "Grey,Amber,Echo,green,red-1"},  // untouched
		{sortUrgency, "Echo,red-1,Amber,green,Grey"}, // error -> red -> amber -> green -> grey
		{sortName, "Amber,Echo,green,Grey,red-1"},    // case-insensitive A-Z
	} {
		rows := clone()
		sortRows(rows, tc.mode)
		if got := strings.Join(names(rows), ","); got != tc.want {
			t.Errorf("sortRows(%v) = %q, want %q", tc.mode, got, tc.want)
		}
	}
	// sortConfig must not reorder the caller's slice contents.
	if got := strings.Join(names(src), ","); got != "Grey,Amber,Echo,green,red-1" {
		t.Errorf("source slice mutated by sortConfig: %q", got)
	}
}

func TestMatchRepo(t *testing.T) {
	meta := rowMeta{region: "fsn1", labels: []string{"high", "home"}}
	for _, tc := range []struct {
		name, q string
		want    bool
	}{
		{"homeserver", "", true},     // empty query matches everything
		{"homeserver", "serv", true}, // name substring
		{"Homeserver", "home", true}, // case-insensitive name
		{"laptop", "fsn1", true},     // region
		{"laptop", "high", true},     // label value
		{"laptop", "prod", false},    // no field matches
	} {
		if got := matchRepo(tc.name, meta, tc.q); got != tc.want {
			t.Errorf("matchRepo(%q, %q) = %v, want %v", tc.name, tc.q, got, tc.want)
		}
	}
}

// `/` opens the filter input, typing narrows the list to matching repos, and the
// header reflects the narrowed count.
func TestFilterNarrowsByName(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	if !m.filtering {
		t.Fatal("/ should open the filter input")
	}
	m = typeFilter(t, m, "repo-b")
	if got := visNames(m); got != "repo-b" {
		t.Fatalf("visible rows = %q, want repo-b", got)
	}
	v := m.View().Content
	if !strings.Contains(v, "1 of 2 repos") {
		t.Errorf("header missing narrowed count\n---\n%s", v)
	}
	if !strings.Contains(v, "/repo-b") {
		t.Errorf("footer missing the filter prompt\n---\n%s", v)
	}
}

// Filtering also matches a repo's labels, not just its name.
func TestFilterMatchesLabel(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	m = typeFilter(t, m, "home") // repo-a has label env=home; repo-b has no labels
	if got := visNames(m); got != "repo-a" {
		t.Fatalf("filter 'home' -> %q, want repo-a", got)
	}
}

// While the filter input is open every key is literal text ("q" must not quit,
// "r" must not refresh), and backspace edits the query.
func TestFilterCapturesKeysAndBackspace(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))

	next, cmd := m.Update(press("q"))
	m = next.(Model)
	if m.quitting || cmd != nil {
		t.Fatal("q while filtering should not quit")
	}
	m = typeFilter(t, m, "rb") // now filter == "qrb"
	if m.filter != "qrb" {
		t.Fatalf("filter = %q, want qrb", m.filter)
	}
	if len(m.pending) != 0 {
		t.Errorf("a refresh was triggered while typing: %v", m.pending)
	}
	m = update(t, m, press("backspace"))
	m = update(t, m, press("backspace"))
	if m.filter != "q" {
		t.Errorf("after two backspaces filter = %q, want q", m.filter)
	}
}

// Enter applies the filter (keeps the query, leaves input mode); esc clears it.
func TestFilterApplyAndClear(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	m = typeFilter(t, m, "repo-a")
	m = update(t, m, press("enter"))
	if m.filtering {
		t.Error("enter should leave the filter input mode")
	}
	if m.filter != "repo-a" {
		t.Errorf("enter should keep the query, got %q", m.filter)
	}
	if got := visNames(m); got != "repo-a" {
		t.Errorf("applied filter visible = %q, want repo-a", got)
	}

	m = update(t, m, press("/")) // reopen with the query still set
	m = update(t, m, press("esc"))
	if m.filtering {
		t.Error("esc should leave the filter input mode")
	}
	if m.filter != "" {
		t.Errorf("esc should clear the query, got %q", m.filter)
	}
	if got := visNames(m); got != "repo-a,repo-b" {
		t.Errorf("after clearing, visible = %q, want both repos", got)
	}
}

// ctrl+c quits even while the filter input is open.
func TestFilterCtrlCStillQuits(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !next.(Model).quitting {
		t.Error("ctrl+c should quit while filtering")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c did not produce tea.QuitMsg")
	}
}

// A filter that matches nothing shows a message and leaves no row selectable.
func TestFilterNoMatch(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	m = typeFilter(t, m, "zzz")
	if _, ok := m.currentRow(); ok {
		t.Error("currentRow should report no selection when nothing matches")
	}
	if v := m.View().Content; !strings.Contains(v, "no repositories match") {
		t.Errorf("expected an empty-match notice\n---\n%s", v)
	}
}

func TestListViewClipsNarrowTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(Model) Model
	}{
		{
			name: "no match",
			setup: func(m Model) Model {
				m.filter = strings.Repeat("very-long-filter", 4)
				return m
			},
		},
		{
			name: "window note",
			setup: func(m Model) Model {
				m.height = 8 // one visible repo plus the "showing N-M of T" note
				return m
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, testApp(nil))
			m.width = 10
			m = tc.setup(m)

			for line := range strings.SplitSeq(m.listView(), "\n") {
				if got := lipgloss.Width(line); got > m.width {
					t.Fatalf("line width = %d, want <= %d: %q", got, m.width, line)
				}
			}
		})
	}
}

// A backup that ran far longer than the Took column is wide gets truncated to
// listTookWidth, so it cannot widen the column and push the trailing Labels
// column out of alignment.
func TestListTookCellTruncatesLongDuration(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	start := testNow.Add(-1001 * time.Hour)
	row := app.RepoStatus{
		Name:  "repo-a",
		Stale: true,
		State: model.RepoState{
			LastSnapshot:  testNow.Add(-time.Hour),
			SnapshotCount: 1,
			Snapshots: []model.Snapshot{{
				ID:   "id-long-duration",
				Time: testNow.Add(-time.Hour),
				Summary: &model.SnapshotSummary{
					BackupStart: start,
					BackupEnd:   start.Add(1000 * time.Hour),
				},
			}},
		},
		Status: model.StatusGreen,
	}

	l := computeListLayout(100)
	rendered := m.renderRow(row, l, false, 100)
	// listTookWidth (7) reserves one cell for the ellipsis, so a 1000h+ run
	// truncates with a trailing ellipsis; the row width invariant matters more
	// than the exact digits, but the ellipsis must appear.
	if got := stripANSI(rendered); !strings.Contains(got, "…") {
		t.Errorf("Took column did not truncate long duration\n---\n%s", got)
	}
	if got := lipgloss.Width(rendered); got > 100 {
		t.Errorf("row width = %d, want <= 100: %q", got, rendered)
	}
	// Stale shows as the `*` marker in the 2-cell status area, not as inline text.
	if got := stripANSI(rendered); !strings.Contains(got, "*") {
		t.Errorf("stale repo should show '*' marker in status cell\n---\n%s", got)
	}
}

// The Took cell distinguishes a known zero-duration backup ("<1s") from one
// without a summary (an em dash), so the user can tell an instant backup from
// missing data. The cell value lives in the row, not a free-form summary.
func TestListTookCellDistinguishesZeroFromUnknown(t *testing.T) {
	start := testNow.Add(-time.Hour)
	zero := model.Snapshot{
		ID: "id-zero", Time: start,
		Summary: &model.SnapshotSummary{BackupStart: start, BackupEnd: start},
	}
	if got := tookDuration([]model.Snapshot{zero}); got != "<1s" {
		t.Errorf("tookDuration(zero) = %q, want <1s", got)
	}
	unknown := model.Snapshot{ID: "id-unknown", Time: start}
	if got := tookDuration([]model.Snapshot{unknown}); got != "—" {
		t.Errorf("tookDuration(no summary) = %q, want em-dash", got)
	}
}

// On a wide pane the snapshot table shows its column header and nothing wraps.
func TestDetailViewShowsResponsiveColumns(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m.width, m.height = 120, 40

	wide := m.titleRow(m.detailTitle()) + "\n" + m.detailBody()
	for _, want := range []string{"Time", "Hostname", "Size"} {
		if !strings.Contains(wide, want) {
			t.Errorf("snapshot table missing column header %q\n---\n%s", want, wide)
		}
	}
	assertLinesFit(t, wide, m.width)
}

// snapshotLayout promotes Added at width 92 and Took at width 100, in priority order,
// never surfacing Took without Added, and always keeps host/tags in range.
func TestSnapshotLayoutProgressiveThresholds(t *testing.T) {
	for _, tc := range []struct {
		width               int
		wantAdded, wantTook bool
	}{
		{80, false, false},
		{91, false, false},
		{92, true, false},
		{99, true, false},
		{100, true, true},
		{140, true, true},
	} {
		l := snapshotLayout(tc.width, false)
		if l.showAdded != tc.wantAdded || l.showTook != tc.wantTook {
			t.Errorf("snapshotLayout(%d) = {added:%v took:%v}, want {added:%v took:%v}",
				tc.width, l.showAdded, l.showTook, tc.wantAdded, tc.wantTook)
		}
		if l.showTook && !l.showAdded {
			t.Errorf("snapshotLayout(%d) promoted Took without Added", tc.width)
		}
		if l.host < 8 || l.host > 24 || l.tags < 1 {
			t.Errorf("snapshotLayout(%d) host=%d tags=%d out of range", tc.width, l.host, l.tags)
		}
	}
}

// As the pane widens, Added then Took graduate to table columns and the bottom
// panel sheds only the Added fact once that column carries it. The exact backup
// window stays in the panel because start/end timestamps are more specific than
// the Took column's duration; duration is appended there only while Took is not
// a column, preserving the no-duplication invariant.
func TestDetailViewProgressiveColumnsAndSlimPanel(t *testing.T) {
	backupWindow := "2026-05-23 13:00:00 → 2026-05-23 13:00:28"
	backupWindowWithDuration := backupWindow + " (28s)"
	for _, tc := range []struct {
		name              string
		width             int
		wantColHeaders    []string
		missingColHeaders []string
		wantText          []string
		missingText       []string
	}{
		{
			name:              "narrow keeps every fact in the panel",
			width:             80,
			missingColHeaders: []string{"Added", "Took"},
			wantText:          []string{backupWindowWithDuration, "+5.0 MiB added", "4.0 MiB packed"},
			missingText:       []string{"took 28s"},
		},
		{
			name:              "medium promotes Added only",
			width:             95,
			wantColHeaders:    []string{"Added"},
			missingColHeaders: []string{"Took"},
			wantText:          []string{backupWindowWithDuration, "4.0 MiB packed"},
			missingText:       []string{"took 28s", "+5.0 MiB added"},
		},
		{
			name:           "wide promotes Added and Took",
			width:          110,
			wantColHeaders: []string{"Added", "Took"},
			wantText:       []string{backupWindow, "4.0 MiB packed"},
			missingText:    []string{"took 28s", backupWindowWithDuration, "+5.0 MiB added"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("enter"))
			m.width, m.height = tc.width, 40

			view := m.titleRow(m.detailTitle()) + "\n" + m.detailBody()
			for _, want := range append(tc.wantColHeaders, tc.wantText...) {
				if !strings.Contains(view, want) {
					t.Errorf("detail view missing %q\n---\n%s", want, view)
				}
			}
			for _, miss := range append(tc.missingColHeaders, tc.missingText...) {
				if strings.Contains(view, miss) {
					t.Errorf("detail view unexpectedly contains %q\n---\n%s", miss, view)
				}
			}
			assertLinesFit(t, view, tc.width)
		})
	}
}

// A backup that ran far longer than the Took column is wide gets truncated to
// snapTookWidth, so it can't widen the column and push the trailing Tags column
// out of alignment.
func TestDetailViewTruncatesLongTookColumn(t *testing.T) {
	if got := snapTook(model.Snapshot{Summary: &model.SnapshotSummary{
		BackupStart: testNow, BackupEnd: testNow.Add(1000 * time.Hour),
	}}); got != "1000h…" || lipgloss.Width(got) > snapTookWidth {
		t.Fatalf("snapTook(1000h) = %q (width %d), want %q within %d",
			got, lipgloss.Width(got), "1000h…", snapTookWidth)
	}

	a := detailApp(t)
	cache := a.Cache.(stubCache)
	state := cache.states["repo-a"]
	newest := state.Snapshots[2] // id-newest, the cursor row, has a summary and a tag
	newest.Summary.BackupEnd = newest.Summary.BackupStart.Add(1000 * time.Hour)
	cache.states["repo-a"] = state

	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m.width, m.height = 110, 40 // wide enough for the Took column

	view := m.titleRow(m.detailTitle()) + "\n" + m.detailBody()
	if !strings.Contains(view, "1000h…") {
		t.Errorf("Took column did not truncate the long duration\n---\n%s", view)
	}
	if strings.Contains(view, "1000h00m") {
		t.Errorf("Took column showed the untruncated duration\n---\n%s", view)
	}
	if !strings.Contains(view, "daily") {
		t.Errorf("Tags column dropped after the long Took value\n---\n%s", view)
	}
	assertLinesFit(t, view, m.width)
}

// Every detail line clips to a narrow pane, including the section heading and
// the no-snapshots placeholder, both wider than the narrowest width tested. The
// height is generous: the detail view's fixed meta block sets a minimum usable
// height, so this exercises width only.
func TestDetailViewClipsNarrowTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		toRepo string // "" = the first repo (3 snapshots); else navigate to this repo
		width  int
	}{
		{"with snapshots, heading wider than pane", "", 8},
		{"no snapshots, placeholder wider than pane", "repo-b", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, detailApp(t)) // repo-b has no cached snapshots
			if tc.toRepo == "repo-b" {
				m = update(t, m, press("j")) // cursor: repo-a -> repo-b
			}
			m = update(t, m, press("enter"))
			m.width, m.height = tc.width, 40

			assertLinesFit(t, m.titleRow(m.detailTitle())+"\n"+m.detailBody(), tc.width)
		})
	}
}

func TestDetailViewFitsCompactTerminal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		height    int
		wantPanel bool
	}{
		{"auxiliary rows hidden at minimum height", 15, false},
		{"panel hidden below its minimum height", 17, false},
		{"panel shown when it fits", 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("enter"))
			m.width, m.height = 80, tc.height

			view := m.View().Content
			if got := lipgloss.Height(view); got > tc.height {
				t.Fatalf("view height = %d, want <= %d\n---\n%s", got, tc.height, view)
			}
			if got := strings.Contains(view, "Selected"); got != tc.wantPanel {
				t.Fatalf("selected-snapshot panel visible = %v, want %v\n---\n%s", got, tc.wantPanel, view)
			}
		})
	}
}

func assertLinesFit(t *testing.T, s string, width int) {
	t.Helper()
	for line := range strings.SplitSeq(s, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line width = %d, want <= %d: %q", got, width, line)
		}
	}
}

// `o` cycles config -> urgency -> name -> config, reorders accordingly, and
// keeps the cursor on the same repo across every press. The header names the
// active sort for each non-config mode.
func TestSortCycleReordersAndKeepsSelection(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 2},
		Repos: []config.Repo{
			{Name: "charlie", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour)},
			{Name: "ada", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour)},
			{Name: "boris", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour)},
		},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	states := map[string]model.RepoState{
		// charlie: recent snapshot but a recorded refresh error -> Error status.
		"charlie": {Name: "charlie", RefreshedAt: testNow, LastSnapshot: testNow.Add(-2 * time.Hour), SnapshotCount: 10, LastError: "boom"},
		// ada: 25h old -> Amber (just past expected frequency, within grace).
		"ada": {Name: "ada", RefreshedAt: testNow, LastSnapshot: testNow.Add(-25 * time.Hour), SnapshotCount: 99},
		// boris: 50h old -> Red (past grace, also the staleness winner).
		"boris": {Name: "boris", RefreshedAt: testNow, LastSnapshot: testNow.Add(-50 * time.Hour), SnapshotCount: 2},
	}
	a := &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: states},
		Clock:   fixedClock{testNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{snaps: []model.Snapshot{{Hostname: "h", Time: testNow.Add(-time.Hour)}}},
	}
	m := newTestModel(t, a)
	if got := visNames(m); got != "charlie,ada,boris" {
		t.Fatalf("default order = %q, want config order", got)
	}
	m = update(t, m, press("j")) // select ada (cursor index 1 in config order)
	if r, _ := m.currentRow(); r.Name != "ada" {
		t.Fatalf("cursor should be on ada, got %q", r.Name)
	}

	steps := []struct {
		mode  sortMode
		order string
		label string // header indicator; empty means no sort indicator
	}{
		{sortUrgency, "charlie,boris,ada", "sort: urgency"},
		{sortName, "ada,boris,charlie", "sort: name"},
		{sortConfig, "charlie,ada,boris", ""},
	}
	for _, step := range steps {
		m = update(t, m, press("o"))
		if m.sortMode != step.mode {
			t.Errorf("sortMode = %v, want %v", m.sortMode, step.mode)
		}
		if got := visNames(m); got != step.order {
			t.Errorf("%s order = %q, want %q", step.mode.label(), got, step.order)
		}
		if r, _ := m.currentRow(); r.Name != "ada" {
			t.Errorf("%s lost the selection (cursor=%d, row=%s)", step.mode.label(), m.cursor, r.Name)
		}
		view := m.View().Content
		if step.label == "" {
			if strings.Contains(view, "sort: ") {
				t.Errorf("config mode should clear the sort indicator\n---\n%s", view)
			}
		} else if !strings.Contains(view, step.label) {
			t.Errorf("header missing %q\n---\n%s", step.label, view)
		}
	}
}

// Under a non-config sort, a background refresh that reorders the list must not
// move the selection: the cursor stays on the same repo by name, the same way
// cycleSort anchors it. Regression for the applyRefresh cursor-drift bug.
func TestSortedSelectionSurvivesRefreshReorder(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour)},
	})
	m := newTestModel(t, a)
	m.sortMode = sortUrgency // both Green: stable sort keeps config order, cursor 0 = repo-a
	if r, _ := m.currentRow(); r.Name != "repo-a" {
		t.Fatalf("precondition: cursor should be on repo-a, got %s", r.Name)
	}

	// repo-b becomes Error; the visible order flips to repo-b, repo-a. applyRefresh
	// assigns msg.row directly without re-evaluating status, so the injected row
	// carries an explicit Status.
	errored := app.RepoStatus{
		Name: "repo-b", Status: model.StatusError,
		State: model.RepoState{Name: "repo-b", RefreshedAt: testNow, LastError: "boom"},
	}
	m = update(t, m, repoRefreshedMsg{name: "repo-b", row: errored})

	if got := visNames(m); got != "repo-b,repo-a" {
		t.Fatalf("order after refresh = %q, want repo-b,repo-a", got)
	}
	if r, ok := m.currentRow(); !ok || r.Name != "repo-a" {
		t.Errorf("selection drifted to %q after reorder, want repo-a (cursor=%d)", r.Name, m.cursor)
	}
}

// Repo-scoped actions follow the filtered selection: with only repo-b visible,
// enter opens the detail view pinned to repo-b and refresh acts on repo-b.
func TestFilteredSelectionDrivesActions(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("/"))
	m = typeFilter(t, m, "repo-b")
	m = update(t, m, press("enter")) // applies and... in filter mode enter only applies
	if m.filtering {
		t.Fatal("enter should have applied the filter")
	}
	// A second enter (now in normal list mode) opens the detail view.
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("enter should open the detail view, got %d", m.view)
	}
	if m.detailName != "repo-b" {
		t.Errorf("detail pinned to %q, want repo-b", m.detailName)
	}
	if name, ok := m.actionRepo(); !ok || name != "repo-b" {
		t.Errorf("actionRepo = (%q,%v), want repo-b", name, ok)
	}
}

// The detail view stays pinned to the repo it was opened on, even when a sort by
// urgency reorders the list underneath it (e.g. after a background refresh).
func TestDetailStaysAnchoredAcrossReorder(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-50 * time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-25 * time.Hour)},
	})
	m := newTestModel(t, a)
	m.sortMode = sortUrgency // repo-a (Red) is first, repo-b (Amber) second
	m = update(t, m, press("enter"))
	if m.detailName != "repo-a" {
		t.Fatalf("opened detail on %q, want repo-a", m.detailName)
	}
	// repo-a recovers to Green; under urgency sort repo-b (Amber) would now sort
	// first, so a cursor-based detail view would jump to repo-b.
	fresher := app.RepoStatus{
		Name: "repo-a", Status: model.StatusGreen,
		State: model.RepoState{Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour)},
	}
	m = update(t, m, repoRefreshedMsg{name: "repo-a", row: fresher})
	if row, ok := m.detailRow(); !ok || row.Name != "repo-a" {
		t.Errorf("detail jumped to %q after reorder, want repo-a", row.Name)
	}
}

// computeListLayout promotes Took then Labels in strict priority order: Took
// (7) appears first and may appear alone, Labels only after Took. listLabelsMin
// + 2 = 3 extra cells is the floor for Labels to appear (1 content cell + the
// 2-space separator).
func TestComputeListLayoutProgressiveThresholds(t *testing.T) {
	// baseFixed = gutter(2)+status(2)+gap(1)+name(24)+last(10)+snaps(5)+2*2 = 48.
	// Took promotion needs baseFixed + 7 + 2 = 57.
	// Labels promotion needs Took on AND remaining >= flexMin(3), i.e. width >= 60.
	for _, tc := range []struct {
		width                int
		wantTook, wantLabels bool
	}{
		{40, false, false}, // well below the Took floor
		{50, false, false}, // still below the Took floor
		{56, false, false}, // one cell short of Took's 9-cell cost
		{57, true, false},  // Took promotes; Labels needs another 3 cells
		{59, true, false},  // still Took alone
		{60, true, true},   // Labels promotes with the minimum flex
		{100, true, true},  // wide pane: both columns present
	} {
		l := computeListLayout(tc.width)
		if l.showTook != tc.wantTook || l.showLabels != tc.wantLabels {
			t.Errorf("computeListLayout(%d) = {took:%v labels:%v}, want {took:%v labels:%v}",
				tc.width, l.showTook, l.showLabels, tc.wantTook, tc.wantLabels)
		}
		// Strict priority: Labels never appears without Took. If this fired the
		// shrinking-window path would lose Took before Labels, the opposite of
		// the documented order.
		if l.showLabels && !l.showTook {
			t.Errorf("computeListLayout(%d) promoted Labels without Took", tc.width)
		}
	}
}

// The list header flags the sorted column with the same arrow browse uses:
// sortName marks Name; sortConfig and sortUrgency order by things that aren't
// labeled columns, so they show no arrow (the title's `sort:` part names them).
func TestListHeaderSortIndicator(t *testing.T) {
	l := computeListLayout(100)
	if got := listHeader(l, sortName); !strings.Contains(got, "Name "+browseSortArrow) {
		t.Errorf("name-sorted header should flag the Name column, got %q", got)
	}
	for _, mode := range []sortMode{sortConfig, sortUrgency} {
		if got := listHeader(l, mode); strings.Contains(got, browseSortArrow) {
			t.Errorf("%s-sorted header should carry no arrow, got %q", mode.label(), got)
		}
	}
}

// listHeader and listCells share the same listLayout, so Name starts at the same
// display column in header/data rows, and metric columns share the same right
// edge. The 5-cell prefix (gutter+status+gap) is present in both.
func TestListHeaderAlignsWithRows(t *testing.T) {
	m := newTestModel(t, testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-2 * time.Hour), SnapshotCount: 7,
			Snapshots: []model.Snapshot{{
				ID: "id", Time: testNow.Add(-2 * time.Hour),
				Summary: &model.SnapshotSummary{BackupStart: testNow.Add(-2 * time.Hour), BackupEnd: testNow.Add(-2*time.Hour + 9*time.Second)},
			}},
		},
	}))

	const width = 100
	l := computeListLayout(width)
	if !l.showTook || !l.showLabels {
		t.Fatalf("precondition: width %d should promote both Took and Labels", width)
	}

	header := stripANSI(listHeader(l, sortConfig))
	row := stripANSI(m.renderRow(m.rows[0], l, false, width))

	// visibleOffset returns the display-cell offset of substr in s. Bytes lie
	// because the status cell's glyph is multi-byte UTF-8; lipgloss.Width on
	// the slice before substr gives the true column position.
	visibleOffset := func(s, substr string) int {
		before, _, ok := strings.Cut(s, substr)
		if !ok {
			return -1
		}
		return lipgloss.Width(before)
	}

	// The 5-cell prefix (gutter+status+gap) puts "Name" and "repo-a" both at
	// the same column. Any drift would mean a header/row layout mismatch.
	headerNamePos := visibleOffset(header, "Name")
	rowNamePos := visibleOffset(row, "repo-a")
	if headerNamePos != 5 || rowNamePos != 5 {
		t.Errorf("Name column offsets: header=%d row=%d, want both 5\n--header--\n%s\n--row--\n%s",
			headerNamePos, rowNamePos, header, row)
	}

	// Last, Snaps, and Took are right-aligned: the header label's right edge must
	// match the row value's right edge (header offset + width(label) ==
	// row offset + width(value)).
	for _, col := range []struct{ label, value string }{
		{"Last", "2h ago"},
		{"Snaps", "7"},
		{"Took", "9s"},
	} {
		hi := visibleOffset(header, col.label)
		ri := visibleOffset(row, col.value)
		if hi < 0 || ri < 0 {
			t.Fatalf("missing %q in header (%d) or %q in row (%d)\n--header--\n%s\n--row--\n%s",
				col.label, hi, col.value, ri, header, row)
		}
		if hi+lipgloss.Width(col.label) != ri+lipgloss.Width(col.value) {
			t.Errorf("right-aligned column %q misaligned: header right=%d row right=%d\n--header--\n%s\n--row--\n%s",
				col.label, hi+lipgloss.Width(col.label), ri+lipgloss.Width(col.value), header, row)
		}
	}
}

// The status cell is always 2 cells wide: a colored glyph plus a one-cell
// marker (`*` for stale, blank otherwise). The marker must not widen the row.
func TestRenderRowStaleMarker(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	row := app.RepoStatus{
		Name: "repo-a", Status: model.StatusGreen, Stale: true,
		State: model.RepoState{RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 1},
	}
	rendered := stripANSI(m.renderRow(row, computeListLayout(100), false, 100))
	if !strings.Contains(rendered, statusGlyph(model.StatusGreen)+"*") {
		t.Errorf("stale row should show '%s*' status cell\n---\n%s", statusGlyph(model.StatusGreen), rendered)
	}
}

// While a refresh is pending the spinner already conveys "data is being
// updated," so the stale `*` marker is suppressed to keep the status cell
// quiet. A lock, however, is independent and actionable: `L` must still show.
func TestRenderRowStaleMarkerSuppressedWhilePending(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m.pending["repo-a"] = true
	stale := app.RepoStatus{
		Name: "repo-a", Status: model.StatusGreen, Stale: true,
		State: model.RepoState{RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 1},
	}
	rendered := stripANSI(m.renderRow(stale, computeListLayout(100), false, 100))
	if strings.Contains(rendered, "*") {
		t.Errorf("pending refresh should suppress '*' marker\n---\n%s", rendered)
	}

	locked := testNow.Add(-time.Hour)
	stale.State.LockedSince = &locked
	rendered = stripANSI(m.renderRow(stale, computeListLayout(100), false, 100))
	if !strings.Contains(rendered, "L") {
		t.Errorf("pending refresh must not suppress 'L' marker\n---\n%s", rendered)
	}
}

// An errored repo carries over its last successful RefreshedAt, which often
// reads as stale, but the error glyph and "refresh failed" text already tell
// the freshness story, so the stale `*` marker is suppressed. Without this
// guard the cell renders a jammed-together error-glyph-and-star.
func TestRenderRowStaleMarkerSuppressedOnError(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	row := app.RepoStatus{
		Name: "repo-a", Status: model.StatusError, Stale: true,
		State: model.RepoState{RefreshedAt: testNow.Add(-72 * time.Hour), LastError: "repository does not exist"},
	}
	rendered := stripANSI(m.renderRow(row, computeListLayout(100), false, 100))
	if strings.Contains(rendered, "*") {
		t.Errorf("error row should suppress stale '*' marker\n---\n%s", rendered)
	}
}

// A repo with an active lock shows "L" in the marker cell. Lock wins over
// stale because it's the more actionable signal.
func TestRenderRowLockMarker(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	locked := testNow.Add(-time.Hour)
	row := app.RepoStatus{
		Name: "repo-a", Status: model.StatusGreen, Stale: true,
		State: model.RepoState{RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 1, LockedSince: &locked},
	}
	rendered := stripANSI(m.renderRow(row, computeListLayout(100), false, 100))
	if !strings.Contains(rendered, statusGlyph(model.StatusGreen)+"L") {
		t.Errorf("locked row should show 'L' marker\n---\n%s", rendered)
	}
	if strings.Contains(rendered, statusGlyph(model.StatusGreen)+"*") {
		t.Errorf("lock should win over stale; row shows '*' instead of 'L'\n---\n%s", rendered)
	}
}

// Long Last, Took, and Labels values get truncated by their column rather
// than widening the row past the pane width.
func TestRenderRowFixedCellsDoNotWiden(t *testing.T) {
	a := testApp(nil)
	m := newTestModel(t, a)
	// Inject a long label into rowMeta so listLabelsValue produces a wide string.
	m.meta["repo-a"] = rowMeta{
		labels: []string{strings.Repeat("verylonglabel", 5)},
	}
	start := testNow.Add(-1001 * time.Hour)
	row := app.RepoStatus{
		Name: "repo-a", Status: model.StatusGreen,
		State: model.RepoState{
			RefreshedAt: testNow, LastSnapshot: testNow.Add(-9999 * time.Hour), SnapshotCount: 999999,
			Snapshots: []model.Snapshot{{
				ID: "id", Time: testNow.Add(-time.Hour),
				Summary: &model.SnapshotSummary{BackupStart: start, BackupEnd: start.Add(1000 * time.Hour)},
			}},
		},
	}
	for _, w := range []int{60, 80, 100, 120} {
		l := computeListLayout(w)
		rendered := m.renderRow(row, l, false, w)
		if got := lipgloss.Width(rendered); got > w {
			t.Errorf("renderRow at width %d produced %d-cell row: %q", w, got, rendered)
		}
	}
}

// groupedSections partitions rows by their value for the configured label
// key. Sections come back in case-insensitive order on the group value, with
// the noKey fallback section last for repos that lack the key.
func TestGroupedSectionsOrder(t *testing.T) {
	rows := []app.RepoStatus{
		{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"}, {Name: "delta"},
	}
	meta := map[string]rowMeta{
		"alpha":   {byKey: map[string]string{"category": "business"}},
		"bravo":   {byKey: map[string]string{"category": "personal"}},
		"charlie": {byKey: map[string]string{"category": "business"}},
		"delta":   {byKey: map[string]string{}}, // missing key -> fallback section
	}
	secs := groupedSections(rows, meta, "category", sortConfig)
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3", len(secs))
	}
	if secs[0].title != "business" {
		t.Errorf("first section title = %q, want business", secs[0].title)
	}
	if secs[1].title != "personal" {
		t.Errorf("second section title = %q, want personal", secs[1].title)
	}
	if secs[2].title != "(no category)" || !secs[2].noKey {
		t.Errorf("last section = %+v, want title=\"(no category)\" noKey=true", secs[2])
	}
	if len(secs[0].rows) != 2 {
		t.Errorf("business section should have 2 rows, got %d", len(secs[0].rows))
	}
}

// Values that differ only by case (e.g. "Prod" vs "prod") must come back in
// a deterministic order. Sorting by the lowercase form alone leaves
// case-only ties to map iteration order, which is randomized, letting two
// displayList() calls in the same handler disagree on cursor mapping.
func TestGroupedSectionsCaseOnlyCollisionIsStable(t *testing.T) {
	rows := []app.RepoStatus{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
	}
	meta := map[string]rowMeta{
		"a": {byKey: map[string]string{"env": "Prod"}},
		"b": {byKey: map[string]string{"env": "prod"}},
		"c": {byKey: map[string]string{"env": "Prod"}},
		"d": {byKey: map[string]string{"env": "prod"}},
	}
	// Run repeatedly to exercise different map iteration orders.
	for i := range 50 {
		secs := groupedSections(rows, meta, "env", sortConfig)
		if len(secs) != 2 {
			t.Fatalf("iter %d: got %d sections, want 2", i, len(secs))
		}
		// Byte-order tie-break: "Prod" (uppercase 'P' = 0x50) < "prod" ('p' = 0x70).
		if secs[0].title != "Prod" || secs[1].title != "prod" {
			t.Fatalf("iter %d: got titles %q,%q want Prod,prod", i, secs[0].title, secs[1].title)
		}
	}
}

// When group_by is configured but no repo carries the key, every repo lands
// in a single noKey fallback section rather than disappearing.
func TestGroupedSectionsAllUngrouped(t *testing.T) {
	rows := []app.RepoStatus{{Name: "one"}, {Name: "two"}}
	meta := map[string]rowMeta{
		"one": {byKey: map[string]string{"env": "home"}},
		"two": {byKey: map[string]string{"env": "home"}},
	}
	secs := groupedSections(rows, meta, "category", sortConfig)
	if len(secs) != 1 || secs[0].title != "(no category)" || !secs[0].noKey {
		t.Fatalf("got sections=%+v, want one (no category) fallback section", secs)
	}
	if len(secs[0].rows) != 2 {
		t.Errorf("fallback section should contain both repos, got %d rows", len(secs[0].rows))
	}
}

// In grouped mode the cursor indexes the flattened display order (the same
// order render uses), so the highlighted repo and the acted-on repo match.
func TestGroupedDisplayOrderDrivesSelection(t *testing.T) {
	a := testApp(nil)
	// Config order: repo-a, repo-b. Put repo-a under "personal" so that group
	// renders after "business" (where a new repo-c is placed), proving the
	// display-order index doesn't fall back to config order.
	a.Cfg.Global.GroupBy = []string{"category"}
	a.Cfg.Repos[0].Labels = map[string]string{"category": "personal"}
	a.Cfg.Repos[1].Labels = map[string]string{"category": "business"}
	m := newTestModel(t, a)
	if !m.groupingActive() {
		t.Fatal("grouping should start active when group_by is configured")
	}
	if m.groupIndex != 1 {
		t.Errorf("groupIndex at startup = %d, want 1 (first configured key)", m.groupIndex)
	}
	if got := m.activeGroupKey(); got != "category" {
		t.Errorf("activeGroupKey at startup = %q, want category", got)
	}
	d := m.displayList()
	// Alphabetical section order: business before personal -> repo-b first, then repo-a.
	if len(d.rows) != 2 || d.rows[0].Name != "repo-b" || d.rows[1].Name != "repo-a" {
		t.Fatalf("display rows = %v, want [repo-b repo-a]", names(d.rows))
	}
	// Cursor 0 in grouped mode picks repo-b (the head of the first section),
	// not config-order repo-a.
	if row, ok := m.currentRow(); !ok || row.Name != "repo-b" {
		t.Errorf("currentRow at cursor 0 = %q, want repo-b", row.Name)
	}
	if name, ok := m.actionRepo(); !ok || name != "repo-b" {
		t.Errorf("actionRepo at cursor 0 = %q, want repo-b", name)
	}
}

// With grouping active, enter pins the detail view to the highlighted repo
// and r marks that same repo pending, both act on display-order selection.
func TestGroupedActionsUseHighlightedRepo(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"category"}
	a.Cfg.Repos[0].Labels = map[string]string{"category": "personal"}
	a.Cfg.Repos[1].Labels = map[string]string{"category": "business"}
	m := newTestModel(t, a)

	// Cursor 0 in grouped mode is repo-b (under "business"). Pressing r should
	// mark repo-b pending.
	next, _ := m.Update(press("r"))
	nm := next.(Model)
	if !nm.pending["repo-b"] {
		t.Errorf("r in grouped mode did not mark the highlighted repo (repo-b) pending: %v", nm.pending)
	}
	// Pressing enter pins detail to the highlighted repo.
	m = update(t, m, press("enter"))
	if m.detailName != "repo-b" {
		t.Errorf("enter pinned detail to %q, want repo-b", m.detailName)
	}
}

// Toggling grouping keeps the cursor on the same repo by name even when its
// flattened-order index changes.
func TestGroupToggleAnchorsSelectionByName(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"category"}
	a.Cfg.Repos[0].Labels = map[string]string{"category": "personal"}
	a.Cfg.Repos[1].Labels = map[string]string{"category": "business"}
	m := newTestModel(t, a)

	// In grouped mode, cursor 0 picks repo-b (business section); move to
	// repo-a (personal section, index 1).
	m = update(t, m, press("j"))
	if row, _ := m.currentRow(); row.Name != "repo-a" {
		t.Fatalf("precondition: expected repo-a selected, got %q", row.Name)
	}

	// Cycle past the single configured key to the flat view. Config order
	// returns repo-a, repo-b; the cursor should follow repo-a (now index 0).
	m = update(t, m, press("g"))
	if m.groupingActive() {
		t.Fatal("g should have cycled past the single configured key into the flat view")
	}
	if row, ok := m.currentRow(); !ok || row.Name != "repo-a" {
		t.Errorf("after cycle off, currentRow = %q (cursor=%d), want repo-a", row.Name, m.cursor)
	}

	// Cycle back to the configured key. repo-a should again be at index 1.
	m = update(t, m, press("g"))
	if !m.groupingActive() {
		t.Fatal("g should have cycled back to the configured key")
	}
	if row, ok := m.currentRow(); !ok || row.Name != "repo-a" {
		t.Errorf("after cycle back on, currentRow = %q (cursor=%d), want repo-a", row.Name, m.cursor)
	}
}

// Under sortUrgency with grouping active, both the sort cycle and a background
// refresh that changes the urgency order keep the cursor anchored to the same
// repo by name.
func TestGroupedSortAndRefreshKeepSelection(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-1 * time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastError: "boom"},
	})
	a.Cfg.Global.GroupBy = []string{"category"}
	a.Cfg.Repos[0].Labels = map[string]string{"category": "shared"}
	a.Cfg.Repos[1].Labels = map[string]string{"category": "shared"}
	m := newTestModel(t, a)

	// Same group: order within the section follows config until sort changes it.
	// Move to repo-b.
	m = update(t, m, press("j"))
	if row, _ := m.currentRow(); row.Name != "repo-b" {
		t.Fatalf("precondition: cursor on %q, want repo-b", row.Name)
	}

	// Sort by urgency: repo-b (Error) should come first; cursor follows the name.
	m = update(t, m, press("o"))
	if m.sortMode != sortUrgency {
		t.Fatalf("sortMode = %v, want urgency", m.sortMode)
	}
	if row, ok := m.currentRow(); !ok || row.Name != "repo-b" {
		t.Errorf("after sort, cursor on %q (idx=%d), want repo-b", row.Name, m.cursor)
	}

	// Background refresh: repo-a also becomes Error; in-group urgency ties, so
	// stable sort reverts to config order repo-a, repo-b and the cursor must
	// follow repo-b to index 1.
	errored := app.RepoStatus{
		Name: "repo-a", Status: model.StatusError,
		State: model.RepoState{Name: "repo-a", RefreshedAt: testNow, LastError: "splat"},
	}
	m = update(t, m, repoRefreshedMsg{name: "repo-a", row: errored})
	if row, ok := m.currentRow(); !ok || row.Name != "repo-b" {
		t.Errorf("after refresh-reorder, cursor on %q (idx=%d), want repo-b", row.Name, m.cursor)
	}
}

// Many one-row groups in a short terminal must still fit within the captured
// height: the rendered View should never push the footer past m.height.
func TestGroupedListFitsHeightWithManyGroups(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 2, GroupBy: []string{"category"}},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	// 8 repos, each in its own group, so grouping yields 8 sections.
	cats := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, cat := range cats {
		cfg.Repos = append(cfg.Repos, config.Repo{
			Name: "repo-" + cat, Credential: "c", Endpoint: "https://e",
			Region: "r", BucketLookup: "auto", Bucket: "b",
			ExpectedFrequency: config.Duration(24 * time.Hour),
			Labels:            map[string]string{"category": cat},
		})
	}
	a := &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: map[string]model.RepoState{}},
		Clock:   fixedClock{testNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{},
	}

	m := newTestModel(t, a)
	for _, height := range []int{8, 10, 14, 20} {
		m.width, m.height = 100, height
		view := m.View().Content
		if got := lipgloss.Height(view); got > height {
			t.Errorf("grouped list overflows height: got %d, want <= %d at height=%d\n---\n%s",
				got, height, height, view)
		}
	}
}

// When the rendered budget is two content lines or more, the cursor's group
// heading must be the first rendered line, never replaced by an adjacent data
// row. This covers the "cursor on second/third row of a single group with
// max=2" case where centering would otherwise drop the heading.
func TestGroupedListIncludesHeadingForCursorSection(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 2, GroupBy: []string{"category"}},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	for _, name := range []string{"repo-a", "repo-b", "repo-c", "repo-d"} {
		cfg.Repos = append(cfg.Repos, config.Repo{
			Name: name, Credential: "c", Endpoint: "https://e",
			Region: "r", BucketLookup: "auto", Bucket: "b",
			ExpectedFrequency: config.Duration(24 * time.Hour),
			Labels:            map[string]string{"category": "only"},
		})
	}
	a := &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: map[string]model.RepoState{}},
		Clock:   fixedClock{testNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{},
	}
	m := newTestModel(t, a)
	// listHeight = h - headerRows(1) - 2*gapRows(2) - footerRows(1) = h - 4.
	// We want max := listHeight - listHeaderRows(1) - listScrollNoteRows(1) = 2,
	// so listHeight must be 4 and h must be 8.
	m.width, m.height = 100, 8

	// Cursor on the second data row (repo-b). Centering would naturally land on
	// [repo-a, repo-b] without the heading; the renderer must replace the first
	// slot with the section heading so context is preserved.
	for _, tc := range []struct {
		cursor int
		want   string
	}{
		{cursor: 1, want: "repo-b"},
		{cursor: 2, want: "repo-c"},
	} {
		m.cursor = tc.cursor
		out := stripANSI(m.renderGroupedList(m.displayList(), computeListLayout(100), 100))
		// First line must be the group heading. The "(4)" count is unique to it.
		lines := strings.Split(out, "\n")
		if len(lines) < 2 {
			t.Fatalf("cursor=%d: expected at least 2 rendered lines, got %d:\n%s", tc.cursor, len(lines), out)
		}
		if !strings.Contains(lines[0], "only") || !strings.Contains(lines[0], "(4)") {
			t.Errorf("cursor=%d: first line %q is not the group heading\n--full--\n%s", tc.cursor, lines[0], out)
		}
		// The cursor's row must be on a line after the heading. With max=2 it's
		// directly below; with a larger budget there may be intervening rows.
		found := false
		for _, line := range lines[1:] {
			if strings.Contains(line, tc.want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("cursor=%d: expected %q after the heading; got:\n%s", tc.cursor, tc.want, out)
		}
	}
}

// The first row of a later section must anchor its heading above the cursor,
// excluding the preceding row and separator from a short centered window.
func TestGroupedListAnchorsHeadingAtFirstRowOfSection(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{Parallelism: 2, GroupBy: []string{"category"}},
	}
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	// Two sections, one row each: minimal token stream that exposes the
	// blank-separator bug: [h0, r0, sep, h1, r1].
	for _, r := range []struct{ name, cat string }{
		{"repo-a", "alpha"}, {"repo-b", "bravo"},
	} {
		cfg.Repos = append(cfg.Repos, config.Repo{
			Name: r.name, Credential: "c", Endpoint: "https://e",
			Region: "r", BucketLookup: "auto", Bucket: "b",
			ExpectedFrequency: config.Duration(24 * time.Hour),
			Labels:            map[string]string{"category": r.cat},
		})
	}
	a := &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: map[string]model.RepoState{}},
		Clock:   fixedClock{testNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{},
	}
	m := newTestModel(t, a)
	// Cursor on repo-b, the first (and only) row of the second section.
	// repo-a (alpha) sorts before repo-b (bravo), so the bravo section is
	// second and m.cursor=1 selects repo-b in displayList().rows.
	m.cursor = 1

	// Three content lines, just enough to include the previous section's row
	// in a naive centered window. listHeight = h - 4, max = listHeight - 2 = 3
	// means h = 9.
	m.width, m.height = 100, 9

	out := stripANSI(m.renderGroupedList(m.displayList(), computeListLayout(100), 100))
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 rendered lines, got %d:\n%s", len(lines), out)
	}
	// First line must be the bravo section heading.
	if !strings.Contains(lines[0], "bravo") || !strings.Contains(lines[0], "(1)") {
		t.Errorf("first line %q is not the bravo heading\n--full--\n%s", lines[0], out)
	}
	// repo-b must appear somewhere after the heading.
	found := false
	for _, line := range lines[1:] {
		if strings.Contains(line, "repo-b") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("repo-b missing from window\n%s", out)
	}
	// repo-a (previous section) must NOT leak into the heading line.
	if strings.Contains(lines[0], "repo-a") {
		t.Errorf("previous section row leaked into the heading line\n%s", out)
	}
}

// Cycling g with two configured keys steps key[0] -> key[1] -> flat -> key[0],
// matching the documented order. activeGroupKey() and groupingActive() track
// the cycle at each step.
func TestCycleGroupingThroughKeys(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"env", "criticality"}
	m := newTestModel(t, a)

	if got := m.activeGroupKey(); got != "env" {
		t.Fatalf("startup activeGroupKey = %q, want env", got)
	}
	if !m.groupingActive() {
		t.Fatal("startup should be grouped by the first key")
	}

	m = update(t, m, press("g"))
	if got := m.activeGroupKey(); got != "criticality" {
		t.Errorf("after first g, activeGroupKey = %q, want criticality", got)
	}
	if !m.groupingActive() {
		t.Error("after first g, grouping should still be active on the second key")
	}

	m = update(t, m, press("g"))
	if got := m.activeGroupKey(); got != "" {
		t.Errorf("after second g, activeGroupKey = %q, want \"\" (flat view)", got)
	}
	if m.groupingActive() {
		t.Error("after second g, grouping should be inactive (flat view)")
	}

	m = update(t, m, press("g"))
	if got := m.activeGroupKey(); got != "env" {
		t.Errorf("after third g, activeGroupKey = %q, want env (wrap)", got)
	}
	if !m.groupingActive() {
		t.Error("after third g, grouping should wrap back to the first key")
	}
}

// A one-key configuration behaves like the old toggle: key -> flat -> key -> ...
func TestCycleGroupingSingleKey(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"env"}
	m := newTestModel(t, a)

	if got := m.activeGroupKey(); got != "env" {
		t.Fatalf("startup activeGroupKey = %q, want env", got)
	}

	m = update(t, m, press("g"))
	if m.groupingActive() {
		t.Error("after one g, single-key cycle should reach the flat view")
	}
	if got := m.activeGroupKey(); got != "" {
		t.Errorf("after one g, activeGroupKey = %q, want \"\"", got)
	}

	m = update(t, m, press("g"))
	if got := m.activeGroupKey(); got != "env" {
		t.Errorf("after two g presses, activeGroupKey = %q, want env (wrap)", got)
	}
}

// Selection follows repository name across grouping modes whose section order
// moves the same repository to different flattened indices.
func TestCycleGroupingPreservesCursor(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"env", "criticality"}
	// repo-a: env=zoo (sorts last alphabetically), criticality=alpha (sorts first)
	// repo-b: env=alpha (sorts first), criticality=zoo (sorts last)
	// Under env: [alpha(repo-b), zoo(repo-a)] -> repo-a at index 1
	// Under criticality: [alpha(repo-a), zoo(repo-b)] -> repo-a at index 0
	// Flat: config order [repo-a, repo-b] -> repo-a at index 0
	a.Cfg.Repos[0].Labels = map[string]string{"env": "zoo", "criticality": "alpha"}
	a.Cfg.Repos[1].Labels = map[string]string{"env": "alpha", "criticality": "zoo"}
	m := newTestModel(t, a)

	// Move the cursor to repo-a under env grouping (it is the second row,
	// because "alpha" sorts before "zoo").
	m = update(t, m, press("j"))
	if row, _ := m.currentRow(); row.Name != "repo-a" {
		t.Fatalf("precondition: cursor on %q, want repo-a under env grouping", row.Name)
	}
	if m.cursor != 1 {
		t.Fatalf("precondition: cursor idx = %d, want 1 (repo-a under env)", m.cursor)
	}

	// Each g press: the cursor must still point to repo-a, even though its
	// numeric index changes between env (1) and criticality/flat (0).
	for i, step := range []struct {
		wantKey string
		wantIdx int
	}{
		{"criticality", 0}, // repo-a sorts first under "alpha" section
		{"", 0},            // flat config order, repo-a is index 0
		{"env", 1},         // wrap: repo-a back to zoo section, index 1
	} {
		m = update(t, m, press("g"))
		if got := m.activeGroupKey(); got != step.wantKey {
			t.Errorf("step %d: activeGroupKey = %q, want %q", i, got, step.wantKey)
		}
		if row, ok := m.currentRow(); !ok || row.Name != "repo-a" {
			t.Errorf("step %d (key=%q): cursor on %q (idx=%d), want repo-a",
				i, step.wantKey, row.Name, m.cursor)
		}
		if m.cursor != step.wantIdx {
			t.Errorf("step %d (key=%q): cursor idx = %d, want %d",
				i, step.wantKey, m.cursor, step.wantIdx)
		}
	}
}

// The Labels column hides the value for the currently active group key (its
// value already heads the section), and shows every label again once the cycle
// reaches the flat view.
func TestCycleGroupingHidesActiveKeyFromLabelsColumn(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"env", "criticality"}
	a.Cfg.Repos[0].Labels = map[string]string{"env": "home", "criticality": "high"}
	m := newTestModel(t, a)

	// Active key = env -> Labels column shows only "high" (the criticality value).
	got := listLabelsValue(m.meta["repo-a"], m.activeGroupKey())
	if got != "high" {
		t.Errorf("active key env: Labels = %q, want %q (only criticality)", got, "high")
	}

	// Cycle to criticality -> Labels column shows only "home" (the env value).
	m = update(t, m, press("g"))
	got = listLabelsValue(m.meta["repo-a"], m.activeGroupKey())
	if got != "home" {
		t.Errorf("active key criticality: Labels = %q, want %q (only env)", got, "home")
	}

	// Cycle to flat view -> Labels column shows both, sorted by key.
	m = update(t, m, press("g"))
	got = listLabelsValue(m.meta["repo-a"], m.activeGroupKey())
	if got != "high · home" {
		t.Errorf("flat view: Labels = %q, want %q (both, ordered by key)", got, "high · home")
	}
}

// listTitle must show the active group key when grouping is active and omit
// the group indicator while in flat view, across the full cycle.
func TestCycleGroupingHeaderIndicator(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Global.GroupBy = []string{"env", "criticality"}
	a.Cfg.Repos[0].Labels = map[string]string{"env": "home", "criticality": "high"}
	a.Cfg.Repos[1].Labels = map[string]string{"env": "office", "criticality": "low"}
	m := newTestModel(t, a)
	m.width, m.height = 200, 30

	for i, want := range []struct {
		contains, omits string
	}{
		{contains: "group: env", omits: ""},
		{contains: "group: criticality", omits: ""},
		{contains: "", omits: "group:"},
		{contains: "group: env", omits: ""},
	} {
		header := stripANSI(m.listTitle())
		if want.contains != "" && !strings.Contains(header, want.contains) {
			t.Errorf("step %d: header %q missing %q", i, header, want.contains)
		}
		if want.omits != "" && strings.Contains(header, want.omits) {
			t.Errorf("step %d: header %q should omit %q", i, header, want.omits)
		}
		m = update(t, m, press("g"))
	}
}

// listTitle must show the applied filter query once the filter input closes,
// hide it while the input is open (the footer prompt shows it there), and drop
// it when the filter is cleared.
func TestListTitleFilterIndicator(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m.width, m.height = 200, 30

	if header := stripANSI(m.listTitle()); strings.Contains(header, "filter:") {
		t.Errorf("no filter: header %q should omit filter indicator", header)
	}

	m = update(t, m, press("/"))
	m = update(t, m, press("a"))
	if header := stripANSI(m.listTitle()); strings.Contains(header, "filter:") {
		t.Errorf("while typing: header %q should omit filter indicator", header)
	}

	m = update(t, m, press("enter"))
	if header := stripANSI(m.listTitle()); !strings.Contains(header, `filter: "a"`) {
		t.Errorf("applied: header %q missing %q", header, `filter: "a"`)
	}

	m = update(t, m, press("/"))
	m = update(t, m, press("esc"))
	if header := stripANSI(m.listTitle()); strings.Contains(header, "filter:") {
		t.Errorf("cleared: header %q should omit filter indicator", header)
	}
}

// Pressing g when group_by is unset is a no-op that surfaces a footer notice
// rather than silently changing nothing.
func TestGroupKeyWithoutConfigShowsNotice(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if m.groupingActive() {
		t.Fatal("precondition: grouping should be inactive without group_by")
	}
	m = update(t, m, press("g"))
	if m.groupIndex != 0 {
		t.Errorf("g without group_by should not change groupIndex, got %d", m.groupIndex)
	}
	if m.groupingActive() {
		t.Error("g without group_by should not enable grouping")
	}
	if !strings.Contains(m.statusMsg, "grouping not configured") {
		t.Errorf("statusMsg = %q, want a 'grouping not configured' notice", m.statusMsg)
	}
}

// Help surfaces (overlay, compact footer help) document the `g` binding so a
// user can discover the toggle. The list-context full-help row that owns
// Filter/Sort should also include Group.
func TestGroupHelpSurfaces(t *testing.T) {
	m := newTestModel(t, testApp(nil))

	// Overlay: g appears in the List section.
	left, _ := m.helpColumns()
	var listSection helpSection
	for _, s := range left {
		if s.title == "List" {
			listSection = s
			break
		}
	}
	if listSection.title == "" {
		t.Fatal("help overlay missing List section")
	}
	var hasGroup bool
	for _, e := range listSection.entries {
		if e.keys == "g" && e.desc == "cycle group key" {
			hasGroup = true
		}
	}
	if !hasGroup {
		t.Errorf("List section should document 'g cycle group key', got %+v", listSection.entries)
	}

	// Compact footer (list view ShortHelp) includes Group.
	short := viewHelp{keys: m.keys, view: listView}.ShortHelp()
	var sawShortGroup bool
	for _, b := range short {
		if b.Help().Key == "g" {
			sawShortGroup = true
		}
	}
	if !sawShortGroup {
		t.Errorf("list-view ShortHelp should include Group, got %v", short)
	}
}

// Region is no longer rendered as a column, but `/fsn1` (the region of both
// repos in testApp) must still narrow the list through matchRepo.
func TestHiddenRegionStillFilters(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("/"))
	m = typeFilter(t, m, "fsn1")
	if got := visNames(m); got != "repo-a,repo-b" {
		t.Errorf("filter 'fsn1' visible = %q, want both repos (region still matches)", got)
	}
}

// snapshotInfoApp seeds repo-a with one richly-detailed snapshot and one
// minimal pre-0.17 snapshot so the info modal can be exercised against both
// shapes without leaning on detailApp's broader fixture.
func snapshotInfoApp(t *testing.T) *app.App {
	t.Helper()
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name:          "repo-a",
			RefreshedAt:   testNow,
			LastSnapshot:  testNow.Add(-time.Hour),
			SnapshotCount: 2,
			Hosts:         []string{"homeserver"},
			Snapshots: []model.Snapshot{
				{ID: "id-minimal", ShortID: "sm", Time: testNow.Add(-2 * time.Hour), Hostname: "homeserver"},
				{
					ID: "id-full", ShortID: "sf", Time: testNow.Add(-time.Hour),
					Hostname: "homeserver", Username: "backup-user",
					Tags: []string{"daily", "system"}, ProgramVersion: "restic 0.18.1",
					Parent:   "parent-id-deadbeef",
					Tree:     "tree-id-cafebabe",
					Paths:    []string{"/etc", "/var/lib"},
					Excludes: []string{"*.tmp", "/var/cache"},
					UID:      new(uint32(0)),
					GID:      new(uint32(0)),
					Summary: &model.SnapshotSummary{
						TotalBytesProcessed: 4404019200,
						DataAdded:           new(int64(5242880)),
						DataAddedPacked:     new(int64(4194304)),
						BackupStart:         testNow.Add(-time.Hour),
						BackupEnd:           testNow.Add(-time.Hour + 28*time.Second),
						FilesNew:            new(uint64(12)),
						FilesChanged:        new(uint64(34)),
						FilesUnmodified:     new(uint64(4050)),
						TotalFilesProcessed: new(uint64(4096)),
						DirsNew:             new(uint64(1)),
						DirsChanged:         new(uint64(2)),
						DirsUnmodified:      new(uint64(7)),
						DataBlobs:           new(int64(11)),
						TreeBlobs:           new(int64(3)),
					},
				},
			},
		},
	})
	a.Cfg.Global.ShellPasswordMode = "env"
	return a
}

// enterInfoOnFullSnapshot opens the info modal on the full-summary snapshot of
// snapshotInfoApp. The detail view orders newest-first, so id-full lands at
// snapCursor 0 and `i` from there opens the modal in one step. The window is
// resized to a tall pane first so the body fits without scrolling; tests that
// exercise scrolling shrink the height themselves.
func enterInfoOnFullSnapshot(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 80})
	m = update(t, m, press("enter")) // -> detailView
	m = update(t, m, press("i"))     // -> infoView
	if m.view != infoView {
		t.Fatalf("view = %d, want infoView", m.view)
	}
	return m
}

func TestInfoModalRendersAllSectionsForFullSnapshot(t *testing.T) {
	m := enterInfoOnFullSnapshot(t, newTestModel(t, snapshotInfoApp(t)))
	view := m.View().Content
	for _, want := range []string{
		"info: repo-a · sf", // title: view prefix + repo + snapshot short id
		"? help",
		// Identity
		"Identity", "ID", "id-full", "Short ID", "sf",
		"Parent", "parent-id-deadbeef",
		"Tree", "tree-id-cafebabe",
		"Program", "restic 0.18.1",
		// Source
		"Source", "Hostname", "homeserver",
		"Username", "backup-user",
		"UID", "GID",
		"Tags", "daily, system",
		"Paths", "/etc", "/var/lib",
		"Excludes", "*.tmp", "/var/cache",
		// Backup window
		"Backup window", "Snapshot time", "Start", "End", "Duration", "28s",
		// Churn
		"Churn", "Total bytes", "Data added", "Data added packed",
		"Data blobs", "Tree blobs",
		"Files new", "Files changed", "Files unmodified", "Files total",
		"Dirs new", "Dirs changed", "Dirs unmodified", "Dirs total",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("info modal missing %q\n---\n%s", want, view)
		}
	}
}

func TestInfoModalSkipsSummarySectionsForMinimalSnapshot(t *testing.T) {
	m := newTestModel(t, snapshotInfoApp(t))
	m = update(t, m, press("enter")) // -> detailView
	// Move cursor to the minimal (older) snapshot. detailSnapshots() sorts
	// newest-first, so the minimal one is index 1.
	m = update(t, m, press("j"))
	m = update(t, m, press("i"))
	if m.view != infoView {
		t.Fatalf("view = %d, want infoView", m.view)
	}
	view := m.View().Content
	for _, want := range []string{
		"Identity", "id-minimal", "Source", "Hostname", "homeserver",
		"Backup window", "Snapshot time",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("info modal missing %q\n---\n%s", want, view)
		}
	}
	for _, forbidden := range []string{
		"Churn", "Files total", "Data added", "Tree blobs",
		// the minimal snapshot has no summary, so Start/End/Duration are also absent
		"Start", "End", "Duration",
	} {
		if strings.Contains(view, forbidden) {
			t.Errorf("info modal should not show %q for pre-summary snapshot\n---\n%s", forbidden, view)
		}
	}
}

func TestInfoKeyTogglesAndPreservesDetailMarks(t *testing.T) {
	m := newTestModel(t, snapshotInfoApp(t))
	m = update(t, m, press("enter")) // -> detailView
	m = update(t, m, press("t"))     // mark the cursor snapshot
	if len(m.detailMarks) != 1 {
		t.Fatalf("precondition: 1 mark, got %d", len(m.detailMarks))
	}

	m = update(t, m, press("i")) // open modal
	if m.view != infoView {
		t.Fatalf("after i view = %d, want infoView", m.view)
	}
	if len(m.detailMarks) != 1 {
		t.Errorf("marks dropped on opening info modal: %d, want 1", len(m.detailMarks))
	}

	m = update(t, m, press("i")) // close via i
	if m.view != detailView {
		t.Fatalf("after second i view = %d, want detailView", m.view)
	}
	if len(m.detailMarks) != 1 {
		t.Errorf("marks dropped on closing info via i: %d, want 1", len(m.detailMarks))
	}

	m = update(t, m, press("i")) // reopen
	m = update(t, m, press("q")) // close via q
	if m.view != detailView {
		t.Fatalf("after q view = %d, want detailView", m.view)
	}

	m = update(t, m, press("i"))                                  // reopen
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyEsc, Text: ""}) // close via esc
	if m.view != detailView {
		t.Fatalf("after esc view = %d, want detailView", m.view)
	}
	if len(m.detailMarks) != 1 {
		t.Errorf("marks dropped on closing info via esc: %d, want 1", len(m.detailMarks))
	}
}

func TestInfoKeyIsNoOpWithoutSnapshot(t *testing.T) {
	a := testApp(nil) // both repos have no cached state, so no snapshots
	m := newTestModel(t, a)
	m = update(t, m, press("enter")) // -> detailView (with empty snapshot list)
	if m.view != detailView {
		t.Fatalf("view = %d, want detailView", m.view)
	}
	m = update(t, m, press("i"))
	if m.view != detailView {
		t.Errorf("after i with no snapshot, view = %d, want detailView (no-op)", m.view)
	}
}

// TestInfoFooterStaysMinimalWhenBodyFits locks the symmetric UX rule: when no
// scrolling is needed the footer must NOT advertise up/down; the only key the
// modal exposes there is `q back`. This guards the conditional in viewHelp
// against regressions that would always-on the scroll chip.
func TestInfoFooterStaysMinimalWhenBodyFits(t *testing.T) {
	m := enterInfoOnFullSnapshot(t, newTestModel(t, snapshotInfoApp(t)))
	view := m.View().Content
	if !strings.Contains(view, "back") {
		t.Fatalf("info footer missing back key\n---\n%s", view)
	}
	// The footer is the last non-empty line; a "scroll" chip there means the
	// conditional regressed. Scan the footer line only so body content can
	// never false-positive a raw substring search.
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	footer := lines[len(lines)-1]
	if strings.Contains(footer, "scroll") {
		t.Errorf("info footer should not advertise scroll keys when body fits\nfooter: %q", footer)
	}
}

// TestInfoModalScrolls covers the case the user hit: a body taller than the
// terminal must remain reachable via up/down/page navigation. The test uses a
// deliberately short window so the body overflows, then drives j/page-down to
// reveal the bottom-most section ("Churn") that was clipped before scrolling
// was wired.
func TestInfoModalScrolls(t *testing.T) {
	m := newTestModel(t, snapshotInfoApp(t))
	// 20 rows leaves only a handful for the body once header/footer/gap are
	// subtracted; the full snapshot's body is well over that, so windowing kicks
	// in and `Churn` is below the fold initially.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	if m.view != infoView {
		t.Fatalf("view = %d, want infoView", m.view)
	}

	initial := m.View().Content
	if !strings.Contains(initial, "showing lines") {
		t.Fatalf("initial view missing position indicator, modal must report overflow\n---\n%s", initial)
	}
	// Scrollability advertised in the footer (the condensed "up/down scroll"
	// chip), not inline with the body anymore.
	if !strings.Contains(initial, "scroll") {
		t.Errorf("info footer missing the scroll chip while scrolling is needed\n---\n%s", initial)
	}
	if strings.Contains(initial, "Churn") {
		t.Fatalf("precondition: with a short window the Churn section must start off-screen\n---\n%s", initial)
	}

	// Page-down enough times to reach the bottom. clampModalScroll bounds the
	// stored offset, so excess presses are a no-op once the floor is hit.
	for range 20 {
		m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	bottom := m.View().Content
	if !strings.Contains(bottom, "Churn") {
		t.Errorf("after scrolling to the bottom, Churn still hidden\n---\n%s", bottom)
	}

	// Page-up returns to the top.
	for range 20 {
		m = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	if m.infoScroll != 0 {
		t.Errorf("after paging back up, infoScroll = %d, want 0", m.infoScroll)
	}

	// j/k must also scroll (they share keys.Up/Down bindings).
	m = update(t, m, press("j"))
	if m.infoScroll != 1 {
		t.Errorf("after j, infoScroll = %d, want 1", m.infoScroll)
	}
	m = update(t, m, press("k"))
	if m.infoScroll != 0 {
		t.Errorf("after k, infoScroll = %d, want 0", m.infoScroll)
	}
}

func TestInfoFooterScrollabilityAccountsForStatusRow(t *testing.T) {
	m := enterInfoOnFullSnapshot(t, newTestModel(t, snapshotInfoApp(t)))
	s := m.selectedSnapshot()
	if s == nil {
		t.Fatal("precondition: selected snapshot missing")
	}
	w, _ := m.effSize()
	lines := m.infoBodyLines(*s, w)
	m.statusMsg = "cache write failed: disk full"
	// This height gives exactly enough room for the body with a one-line footer.
	// The status message makes the real footer two lines, so the body must
	// overflow and the help row must advertise scroll keys.
	m.height = headerRows + 2*gapRows + 1 + len(lines)

	view := m.View().Content
	if !strings.Contains(view, "showing lines") {
		t.Fatalf("info body should show a scroll hint once status adds a footer row\n---\n%s", view)
	}
	plain := stripANSI(view)
	rows := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	footer := rows[len(rows)-1]
	if !strings.Contains(footer, "scroll") {
		t.Errorf("info footer missing the scroll chip with status row\nfooter: %q\n---\n%s", footer, plain)
	}
}

// Closing and reopening the modal must reset the scroll position so the user
// always lands at the top of the new snapshot's body.
func TestInfoModalScrollResetsOnOpen(t *testing.T) {
	m := newTestModel(t, snapshotInfoApp(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	for range 5 {
		m = update(t, m, press("j"))
	}
	if m.infoScroll == 0 {
		t.Fatalf("precondition: infoScroll should be > 0 after scrolling")
	}
	m = update(t, m, press("i")) // close
	m = update(t, m, press("i")) // reopen
	if m.infoScroll != 0 {
		t.Errorf("infoScroll = %d on reopen, want 0", m.infoScroll)
	}
}

// TestRepoCommandKeysAreIgnoredByInfoModal mirrors the help-overlay test:
// behind the modal `s`/`r`/`R` must not launch repo actions.
func TestRepoCommandKeysAreIgnoredByInfoModal(t *testing.T) {
	m := enterInfoOnFullSnapshot(t, newTestModel(t, snapshotInfoApp(t)))
	for _, k := range []string{"s", "r", "R"} {
		next, cmd := m.Update(press(k))
		nm := next.(Model)
		if cmd != nil {
			t.Errorf("key %q in info modal produced a command, want none", k)
		}
		if len(nm.pending) != 0 {
			t.Errorf("key %q in info modal started a refresh (pending=%v), want none", k, nm.pending)
		}
		if nm.view != infoView {
			t.Errorf("key %q left the info modal, view = %d, want infoView", k, nm.view)
		}
	}
}

// TestInfoViewHelpHasExplicitCase locks that viewHelp's ShortHelp/FullHelp
// return an info-specific case rather than falling through to the list-view
// default (which would advertise the wrong keys behind the modal footer).
func TestInfoViewHelpHasExplicitCase(t *testing.T) {
	k := defaultKeys()
	short := viewHelp{keys: k, view: infoView}.ShortHelp()
	full := viewHelp{keys: k, view: infoView}.FullHelp()
	listShort := viewHelp{keys: k, view: listView}.ShortHelp()

	if len(short) == len(listShort) {
		t.Errorf("info ShortHelp same length as list ShortHelp (%d), expected a distinct case", len(short))
	}
	if len(full) == 0 {
		t.Fatalf("info FullHelp is empty")
	}
	// info footer should advertise close keys (Info / Back) but not the detail
	// scope actions like Mark or Diff.
	for _, b := range short {
		desc := b.Help().Desc
		if desc == "mark" || desc == "diff" {
			t.Errorf("info ShortHelp includes detail action %q", desc)
		}
	}
}

func TestTruncateWidth(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"}, // fits unchanged
		{"hello", 5, "hello"},  // exact fit
		{"hello", 4, "hel…"},   // truncated with ellipsis
		{"hello", 1, "…"},      // single cell is just the ellipsis
		{"hello", 0, ""},       // non-positive width yields empty, not the input
		{"hello", -3, ""},      // negative likewise
		{"héllo", 3, "hé…"},    // combining-free accent is one cell wide
		{"中文档", 4, "中…"},       // wide runes measured by display width, not rune count
		{"中文", 3, "中…"},        // a 4-cell value clipped into a 3-cell budget
		{"", 5, ""},            // empty input stays empty
	}
	for _, tt := range tests {
		got := truncateWidth(tt.s, tt.max)
		if got != tt.want {
			t.Errorf("truncateWidth(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
		if w := lipgloss.Width(got); tt.max > 0 && w > tt.max {
			t.Errorf("truncateWidth(%q, %d) is %d cells wide, over budget", tt.s, tt.max, w)
		}
	}
}

// snapCells must pad the Hostname column by display width, not rune count: a
// wide-rune hostname trimmed to <= l.host cells by truncateWidth still has fewer
// runes than cells, so fmt's %-*s would pad it back past l.host and shove
// Size/Added/Took/Tags out of alignment. The cell must stay exactly l.host cells
// regardless of how many runes that is.
func TestSnapCellsHostCellWidth(t *testing.T) {
	l := snapLayout{idWidth: snapIDWidth, host: 10, tags: 20, showAdded: true, showTook: true}
	for _, host := range []string{
		"buildhost", // ASCII: rune count already equals cell width
		"中文档中文档主机",  // wide runes: 2 cells each, trimmed below the rune count
		"サーバー東京",    // CJK that needs truncation to fit
	} {
		cell := snapCells(l, snapRow{host: truncateWidth(host, l.host)})[2]
		if w := lipgloss.Width(cell); w != l.host {
			t.Errorf("snapCells host cell for %q is %d cells, want exactly %d", host, w, l.host)
		}
	}
}

// truncateNameWidth keeps the extension (and a dir's trailing slash) visible
// through the cut and elides the stem in the middle, so a column of truncated
// names still tells file types apart and keeps the tail where generated and
// versioned names actually differ.
func TestTruncateNameWidth(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"short.pdf", 20, "short.pdf"},                            // fits unchanged
		{"a-very-long-document-name.pdf", 16, "a-very…-name.pdf"}, // extension and stem tail survive
		{"VID-20200507-WA0012.mp4", 12, "VID-…012.mp4"},           // sequence tail beats the shared prefix
		{"a-very-long-directory-name/", 12, "a-ver…-name/"},       // dirs keep the trailing slash
		{"no-extension-at-all-here", 12, "no-ext…-here"},          // no ext: stem still middle-elided
		{".config-cache-2024-backup", 12, ".confi…ackup"},         // dotfile: leading dot is no ext
		{"name.with a space.suffix here", 12, "name.w… here"},     // spaced suffix is no ext
		{"long-name.backupfile", 14, "long-na…upfile"},            // overlong suffix is no ext
		{"x.pdf", 4, "x.p…"},                                      // too narrow for the suffix: plain cut
		{"héllo-wörld.txt", 10, "hél…ld.txt"},                     // width-measured head and tail
		{"中文文档备份记录.txt", 12, "中文…录.txt"},                          // double-cell runes: the tail drops one that won't fit its last cell
	}
	for _, tt := range tests {
		got := truncateNameWidth(tt.s, tt.max)
		if got != tt.want {
			t.Errorf("truncateNameWidth(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
		if w := lipgloss.Width(got); w > tt.max {
			t.Errorf("truncateNameWidth(%q, %d) is %d cells wide", tt.s, tt.max, w)
		}
	}
}

// truncatePathWidth keeps the basename intact and elides the middle of the
// directory chain, keeping as many leading components as fit; when even
// ".../<base>" overflows it falls back to extension-preserving name truncation,
// and a slash-free input behaves exactly like truncateNameWidth.
func TestTruncatePathWidth(t *testing.T) {
	const long = "/Android/media/com.whatsapp/WhatsApp/Media/IMG-1234.jpg"
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{long, 60, long}, // fits unchanged
		{long, 40, "/Android/media/…/IMG-1234.jpg"},        // middle elided at component boundaries
		{long, 14, "…/IMG-1234.jpg"},                       // only the basename fits
		{long, 10, "…/I…34.jpg"},                           // basename itself cut, extension kept
		{"/a/b/c/dir/", 8, "/…/dir/"},                      // dir paths keep the trailing slash
		{"bare-name-no-slashes.txt", 12, "bare…hes.txt"},   // no slash: name truncation
		{"  /home/deep/needle.txt", 18, "  /…/needle.txt"}, // icon prefix sticks to the head
	}
	for _, tt := range tests {
		got := truncatePathWidth(tt.s, tt.max)
		if got != tt.want {
			t.Errorf("truncatePathWidth(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
		if w := lipgloss.Width(got); w > tt.max {
			t.Errorf("truncatePathWidth(%q, %d) is %d cells wide", tt.s, tt.max, w)
		}
	}
}
