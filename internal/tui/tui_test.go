package tui

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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

type stubRestic struct {
	snaps       []model.Snapshot
	browseNodes []model.BrowseNode // streamed by StreamSnapshotTree
	browseErr   error              // returned after streaming (e.g. a restic failure)
	browseDelay time.Duration      // optional per-node delay to model a slow crawl
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
	return model.BrowseScanSummary{Entries: len(s.browseNodes), Complete: true}, nil
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

var testNow = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

func int64p(n int64) *int64 { return &n }

func uint64p(n uint64) *uint64 { return &n }

func testApp(states map[string]model.RepoState) *app.App {
	if states == nil {
		states = map[string]model.RepoState{}
	}
	cfg := &config.Config{
		Global:      config.Global{Parallelism: 2},
		Credentials: []config.Credential{{Name: "cred-a"}},
		Repos: []config.Repo{
			{Name: "repo-a", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b", ExpectedFrequency: config.Duration(24 * time.Hour),
				Labels: map[string]string{"env": "home", "criticality": "high"}},
			{Name: "repo-b", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b2", ExpectedFrequency: config.Duration(24 * time.Hour)},
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

// leafCmds flattens a possibly-batched command into its leaf commands. The
// refresh keys now batch the spinner tick with the live refresh, so a test that
// wants the refresh command unwraps the batch first. Running the batch wrapper
// only assembles the slice — it never runs the leaves — so this is safe even
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

// --- tests ---

func TestViewRendersReposGlyphsAndMeta(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-2 * time.Hour), SnapshotCount: 240},
	})
	m := newTestModel(t, a)
	view := m.View().Content

	for _, want := range []string{
		"resticscope", "restic 0.18.1",
		"repo-a", "repo-b",
		statusGlyph(model.StatusGreen), // repo-a is green
		statusGlyph(model.StatusGrey),  // repo-b never refreshed
		"240",
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
// invisible, so re-arming its tick only re-renders a hidden frame ~12×/second
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
	// r batches the spinner tick with the refresh; run every leaf (the tick
	// returns at once, only the refresh blocks) and forward just the refresh
	// result so the cancel assertion below isn't satisfied by the tick.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		c := c
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
// context plus a status-glyph legend; `?` again closes it back to the list.
func TestHelpOverlayToggle(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	if m.view != listView {
		t.Fatalf("view starts at %d, want listView", m.view)
	}
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
		"never refreshed",   // glyph legend entry
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

func TestHelpHeaderClipsNarrowTerminal(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m.width = 12
	header := m.helpHeaderView()
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

// The footers and detail header must advertise the new back/quit scheme: the
// list still shows `q quit`, the detail header and both nested footers show
// `q back`, and the removed `b back` hint appears nowhere. The footer's help
// styles each key and label as separate ANSI spans, so we strip color before
// matching the "key label" pairs.
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
	if header := stripANSI(m.detailHeaderView()); !strings.Contains(header, "q back") {
		t.Errorf("detail header should show 'q back'\n---\n%s", header)
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

// --- detail view ---

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
						TotalBytesProcessed: 4404019200, DataAdded: int64p(5242880), DataAddedPacked: int64p(4194304),
						BackupStart: testNow.Add(-1 * time.Hour), BackupEnd: testNow.Add(-1*time.Hour + 28*time.Second),
						FilesNew: uint64p(12), FilesChanged: uint64p(34), TotalFilesProcessed: uint64p(4096),
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
		"Endpoint",         // metadata block
		"Versions",         // meta rollup of observed restic versions
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
}

func TestSnapshotChurnOmitsMissingFields(t *testing.T) {
	got := snapshotChurn(&model.SnapshotSummary{DataAdded: int64p(5242880)}, true)
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
		DataAdded: int64p(5242880), DataAddedPacked: int64p(4194304),
		FilesNew: uint64p(12), FilesChanged: uint64p(34), TotalFilesProcessed: uint64p(4096),
	}
	got := snapshotChurn(sum, false)
	if want := "4.0 MiB packed · 12 new · 34 changed · 4096 files"; got != want {
		t.Errorf("snapshotChurn(includeAdded=false) = %q, want %q", got, want)
	}
	if strings.Contains(got, "added") || strings.Contains(got, "(") {
		t.Errorf("snapshotChurn(includeAdded=false) should drop the added piece and its parens: %q", got)
	}
	// Only added bytes present: nothing remains once Added owns it.
	if got := snapshotChurn(&model.SnapshotSummary{DataAdded: int64p(5242880)}, false); got != "—" {
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
	// Narrow pane: no Took column, so the duration stays in the panel heading,
	// which is where we prove the selected detail tracks the ordered snapshot.
	m.width, m.height = 80, 40
	snaps := m.detailSnapshots()
	if len(snaps) != 2 || snaps[0].ID != "b" {
		t.Fatalf("detailSnapshots first = %+v, want ID b", snaps)
	}
	if snap := m.selectedSnapshot(); snap == nil || snap.ID != "b" {
		t.Fatalf("selectedSnapshot = %+v, want ID b", snap)
	}
	if !strings.Contains(m.View().Content, "took 20s") {
		t.Fatalf("detail view did not use the same ordered snapshot for selected detail:\n%s", m.View().Content)
	}
}

func TestDetailBackReturnsToList(t *testing.T) {
	for _, k := range []string{"esc", "q"} {
		m := newTestModel(t, detailApp(t))
		next, cmd := m.Update(press("enter"))
		m = next.(Model)
		if m.view != detailView {
			t.Fatal("expected detail view after enter")
		}
		next, cmd = m.Update(press(k))
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

// b now opens the in-app file browser for the selected snapshot: it switches to
// browseView (showing the indexing state with no listing yet), marks the index in
// flight, and returns the command that runs the one-time index.
func TestDetailBKeyStartsBrowse(t *testing.T) {
	m := newTestModel(t, browseApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatal("expected detail view after enter")
	}
	next, cmd := m.Update(press("b"))
	nm := next.(Model)
	if nm.view != browseView {
		t.Errorf("b should open the browse view; view = %d", nm.view)
	}
	if !nm.browseLoading || nm.browseIndexed {
		t.Errorf("b should mark the index in flight, not yet indexed: loading=%v indexed=%v", nm.browseLoading, nm.browseIndexed)
	}
	if nm.browseRepo != "repo-a" || nm.browseSnapshot != "id-newest" {
		t.Errorf("browse target = %q/%q, want repo-a/id-newest", nm.browseRepo, nm.browseSnapshot)
	}
	if cmd == nil {
		t.Error("b should emit a browse command")
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

// i opens the full snapshot-details modal, but only from the detail view and
// only when a snapshot exists under the cursor.
func TestSnapInfoOpensOnlyFromDetailWithSnapshot(t *testing.T) {
	// From the list view, i does nothing (the binding is detail-scoped).
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("i"))
	if m.view != listView {
		t.Errorf("i from the list should not open snapshot info, view = %d", m.view)
	}

	// repo-b has no cached snapshots; i in its detail view must stay put.
	m = newTestModel(t, detailApp(t))
	m = update(t, m, press("j"))     // cursor: repo-a -> repo-b
	m = update(t, m, press("enter")) // open repo-b detail (no snapshots)
	m = update(t, m, press("i"))
	if m.view != detailView {
		t.Errorf("i with no snapshot should not open the modal, view = %d", m.view)
	}

	// repo-a has snapshots; i opens the modal.
	m = newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	if m.view != snapInfoView {
		t.Errorf("i from detail with a snapshot should open snapInfoView, view = %d", m.view)
	}
}

// i, esc, and q all close the modal back to the detail view (not the list),
// without setting quitting or emitting a command.
func TestSnapInfoCloseKeysReturnToDetail(t *testing.T) {
	for _, k := range []string{"i", "esc", "q"} {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter"))
		m = update(t, m, press("i"))
		if m.view != snapInfoView {
			t.Fatalf("precondition: i should open snapInfoView, view = %d", m.view)
		}
		next, cmd := m.Update(press(k))
		nm := next.(Model)
		if nm.view != detailView {
			t.Errorf("%q should close the modal to the detail view, view = %d", k, nm.view)
		}
		if nm.quitting {
			t.Errorf("%q from the modal should not set quitting", k)
		}
		if cmd != nil {
			t.Errorf("%q from the modal should emit no command", k)
		}
	}
}

// The snapshot-info modal is fully modal: every action key behind it — including
// the help toggle ?, refresh, shell, browse, and enter — does nothing and leaves
// the modal open.
func TestSnapInfoSwallowsActionKeys(t *testing.T) {
	for _, k := range []string{"?", "R", "r", "s", "b", "enter"} {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter"))
		m = update(t, m, press("i"))
		next, cmd := m.Update(press(k))
		nm := next.(Model)
		if nm.view != snapInfoView {
			t.Errorf("%q should not leave the snapshot-info modal, view = %d", k, nm.view)
		}
		if len(nm.pending) != 0 {
			t.Errorf("%q should not start a refresh behind the modal: %v", k, nm.pending)
		}
		if cmd != nil {
			t.Errorf("%q behind the modal should emit no command", k)
		}
	}
}

// ctrl+c still hard-quits from inside the modal.
func TestSnapInfoCtrlCStillQuits(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !next.(Model).quitting {
		t.Error("ctrl+c should quit from the snapshot-info modal")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c did not produce tea.QuitMsg")
	}
}

// The modal renders the full record in hand, including the newly surfaced
// Username and every summary field, formatted from real data (no Go zero time).
func TestSnapInfoRendersFullRecord(t *testing.T) {
	a := detailApp(t)
	cache := a.Cache.(stubCache)
	state := cache.states["repo-a"]
	state.Snapshots[2].Username = "backup-user" // id-newest, the cursor row
	cache.states["repo-a"] = state

	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))

	view := m.View().Content
	for _, want := range []string{
		"snapshot s3",     // header short id
		"i/esc close",     // close hint
		"id-newest",       // full id
		"backup-user",     // newly surfaced Username
		"restic 0.18.1",   // version
		"daily",           // tags
		"12 new",          // files new
		"34 changed",      // files changed
		"4096 total",      // total files
		"4.1 GiB logical", // logical size (4404019200 bytes)
		"+5.0 MiB added",  // deduped bytes
		"4.0 MiB packed",  // packed bytes
		"28s",             // backup duration
	} {
		if !strings.Contains(view, want) {
			t.Errorf("snapshot-info modal missing %q\n---\n%s", want, view)
		}
	}
}

// A snapshot without a summary (pre-0.17) renders "no summary" rather than
// panicking on the nil summary pointer.
func TestSnapInfoNilSummary(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 1,
			Snapshots: []model.Snapshot{
				{ID: "id-old", ShortID: "old", Time: testNow.Add(-time.Hour), Hostname: "h"},
			},
		},
	})
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	if !strings.Contains(m.View().Content, "no summary") {
		t.Errorf("modal should show 'no summary' for a summary-less snapshot\n---\n%s", m.View().Content)
	}
}

// A non-nil summary with zero backup timestamps must not format Go's zero time;
// it shows the backup window as unavailable instead.
func TestSnapInfoZeroBackupWindow(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour), SnapshotCount: 1,
			Snapshots: []model.Snapshot{
				{ID: "id-z", ShortID: "z", Time: testNow.Add(-time.Hour), Hostname: "h",
					Summary: &model.SnapshotSummary{TotalBytesProcessed: 1024}},
			},
		},
	})
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	view := m.View().Content
	if strings.Contains(view, "0001-01-01") || strings.Contains(view, "00:00:00") {
		t.Errorf("modal formatted Go zero time for an empty backup window\n---\n%s", view)
	}
	if !strings.Contains(view, "unavailable") {
		t.Errorf("modal should mark the missing backup window as unavailable\n---\n%s", view)
	}
}

// A backup that crosses midnight (or spans days) must render the end timestamp
// with its own full date, not a bare clock time, so the completion date is never
// ambiguous.
func TestSnapInfoBackupWindowCrossesDate(t *testing.T) {
	start := time.Date(2026, 5, 23, 23, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 24, 0, 10, 0, 0, time.UTC)
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: start, SnapshotCount: 1,
			Snapshots: []model.Snapshot{
				{ID: "id-cross", ShortID: "x", Time: start, Hostname: "h",
					Summary: &model.SnapshotSummary{
						TotalBytesProcessed: 1024, BackupStart: start, BackupEnd: end,
					}},
			},
		},
	})
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	view := m.View().Content
	if !strings.Contains(view, "2026-05-23 23:30:00 → 2026-05-24 00:10:00") {
		t.Errorf("modal should show the end date on a midnight-crossing backup\n---\n%s", view)
	}
}

// A refresh that empties the detail repo's snapshots while the modal is open must
// not panic; it renders the path-free nil-selection fallback.
func TestSnapInfoSurvivesSnapshotDropDuringRefresh(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m = update(t, m, press("i"))
	if m.view != snapInfoView {
		t.Fatalf("precondition: modal should be open, view = %d", m.view)
	}

	// A background refresh replaces repo-a with an empty snapshot list.
	emptied := app.RepoStatus{Name: "repo-a", Status: model.StatusGreen,
		State: model.RepoState{Name: "repo-a", RefreshedAt: testNow}}
	m = update(t, m, repoRefreshedMsg{name: "repo-a", row: emptied})

	view := m.View().Content // must not panic
	if !strings.Contains(view, "no snapshot selected") {
		t.Errorf("modal should render the nil-selection fallback after the snapshot drops\n---\n%s", view)
	}
	if strings.Contains(stripANSI(m.snapInfoHeaderView()), "snapshot id-") {
		t.Errorf("header should drop the stale short id when no snapshot remains")
	}
}

// The full `?` help overlay opened from the detail view documents the i binding
// under the Detail section.
func TestHelpOverlayDocumentsSnapInfo(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	_, right := m.helpColumns()
	var detail helpSection
	for _, s := range right {
		if s.title == "Detail" {
			detail = s
		}
	}
	found := false
	for _, e := range detail.entries {
		if e.keys == "i" && e.desc == "snapshot info" {
			found = true
		}
	}
	if !found {
		t.Errorf("Detail help section should document 'i snapshot info', got %+v", detail.entries)
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

// --- filter & sort ---

func names(rows []app.RepoStatus) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func visNames(m Model) string { return strings.Join(names(m.visibleRows()), ",") }

func typeFilter(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = update(t, m, press(string(r)))
	}
	return m
}

func TestSortRowsOrders(t *testing.T) {
	base := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	src := []app.RepoStatus{
		{Name: "a", State: model.RepoState{LastSnapshot: base.Add(-1 * time.Hour)}},
		{Name: "b", State: model.RepoState{LastSnapshot: base.Add(-5 * time.Hour)}},
		{Name: "c", State: model.RepoState{}}, // never refreshed: zero time
	}
	clone := func() []app.RepoStatus { return append([]app.RepoStatus(nil), src...) }

	for _, tc := range []struct {
		mode sortMode
		want string
	}{
		{sortConfig, "a,b,c"}, // untouched
		{sortStale, "c,b,a"},  // oldest/never first
	} {
		rows := clone()
		sortRows(rows, tc.mode)
		if got := strings.Join(names(rows), ","); got != tc.want {
			t.Errorf("sortRows(%v) = %q, want %q", tc.mode, got, tc.want)
		}
	}
	// sortConfig must not reorder the caller's slice contents.
	if got := strings.Join(names(src), ","); got != "a,b,c" {
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

// While the filter input is open every key is literal text — "q" must not quit,
// "r" must not refresh — and backspace edits the query.
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

			for _, line := range strings.Split(m.listView(), "\n") {
				if got := lipgloss.Width(line); got > m.width {
					t.Fatalf("line width = %d, want <= %d: %q", got, m.width, line)
				}
			}
		})
	}
}

func TestListSummaryTruncatesLongDuration(t *testing.T) {
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

	got := m.summary(row)
	if !strings.Contains(got, "took: 1000h0…  ") {
		t.Fatalf("summary did not truncate duration to its fixed field:\n%s", got)
	}
	if !strings.Contains(got, "(stale)") {
		t.Fatalf("summary dropped stale marker:\n%s", got)
	}
}

func TestListSummaryDistinguishesZeroDurationFromUnknown(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	start := testNow.Add(-time.Hour)
	row := app.RepoStatus{
		Name: "repo-a",
		State: model.RepoState{
			LastSnapshot:  start,
			SnapshotCount: 1,
			Snapshots: []model.Snapshot{{
				ID:   "id-zero-duration",
				Time: start,
				Summary: &model.SnapshotSummary{
					BackupStart: start,
					BackupEnd:   start,
				},
			}},
		},
		Status: model.StatusGreen,
	}

	if got := m.summary(row); !strings.Contains(got, "took: <1s") {
		t.Fatalf("summary did not show known zero duration as <1s:\n%s", got)
	}

	row.State.Snapshots[0].Summary = nil
	if got := m.summary(row); !strings.Contains(got, "took: —") {
		t.Fatalf("summary did not show unknown duration as em-dash:\n%s", got)
	}
}

// On a wide pane the snapshot table shows its column header and nothing wraps.
func TestDetailViewShowsResponsiveColumns(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	m.width, m.height = 120, 40

	wide := m.detailHeaderView() + "\n" + m.detailBody()
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
		l := snapshotLayout(tc.width)
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
// panel sheds exactly the facts those columns now carry — so the union of
// (columns + panel) loses nothing and duplicates nothing at any width.
func TestDetailViewProgressiveColumnsAndSlimPanel(t *testing.T) {
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
			wantText:          []string{"took 28s", "+5.0 MiB added", "4.0 MiB packed"},
		},
		{
			name:              "medium promotes Added only",
			width:             95,
			wantColHeaders:    []string{"Added"},
			missingColHeaders: []string{"Took"},
			wantText:          []string{"took 28s", "4.0 MiB packed"},
			missingText:       []string{"+5.0 MiB added"},
		},
		{
			name:           "wide promotes Added and Took",
			width:          110,
			wantColHeaders: []string{"Added", "Took"},
			wantText:       []string{"4.0 MiB packed"},
			missingText:    []string{"took 28s", "+5.0 MiB added"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("enter"))
			m.width, m.height = tc.width, 40

			view := m.detailHeaderView() + "\n" + m.detailBody()
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

	view := m.detailHeaderView() + "\n" + m.detailBody()
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

// Every detail line clips to a narrow pane — including the section heading and
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

			assertLinesFit(t, m.detailHeaderView()+"\n"+m.detailBody(), tc.width)
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
		{"panel shown when it fits", 19, true},
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
	for _, line := range strings.Split(s, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line width = %d, want <= %d: %q", got, width, line)
		}
	}
}

// `o` cycles config -> staleness -> config, reorders accordingly, and keeps the
// cursor on the same repo across the reorder. The header names the active sort.
func TestSortCycleReordersAndKeepsSelection(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-1 * time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-9 * time.Hour)},
	})
	m := newTestModel(t, a)
	if got := visNames(m); got != "repo-a,repo-b" {
		t.Fatalf("default order = %q, want config order", got)
	}
	m = update(t, m, press("j")) // select repo-b
	if r, _ := m.currentRow(); r.Name != "repo-b" {
		t.Fatalf("cursor should be on repo-b")
	}

	m = update(t, m, press("o")) // staleness: repo-b (-9h) before repo-a (-1h)
	if m.sortMode != sortStale {
		t.Fatalf("sortMode = %v, want staleness", m.sortMode)
	}
	if got := visNames(m); got != "repo-b,repo-a" {
		t.Errorf("staleness order = %q, want repo-b,repo-a", got)
	}
	if r, _ := m.currentRow(); r.Name != "repo-b" || m.cursor != 0 {
		t.Errorf("sort lost the selection (cursor=%d, row=%s)", m.cursor, r.Name)
	}
	if !strings.Contains(m.View().Content, "sort: staleness") {
		t.Errorf("header should name the active sort")
	}

	m = update(t, m, press("o")) // back to config order
	if m.sortMode != sortConfig || visNames(m) != "repo-a,repo-b" {
		t.Errorf("cycle did not return to config order: mode=%v order=%q", m.sortMode, visNames(m))
	}
}

// Under a non-config sort, a background refresh that reorders the list must not
// move the selection: the cursor stays on the same repo by name, the same way
// cycleSort anchors it. Regression for the applyRefresh cursor-drift bug.
func TestSortedSelectionSurvivesRefreshReorder(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-5 * time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour)},
	})
	m := newTestModel(t, a)
	m.sortMode = sortStale // repo-a (oldest) sorts first, so cursor 0 is repo-a
	if r, _ := m.currentRow(); r.Name != "repo-a" {
		t.Fatalf("precondition: cursor should be on repo-a, got %s", r.Name)
	}

	// repo-b ages past repo-a; the visible order flips to repo-b, repo-a.
	older := app.RepoStatus{Name: "repo-b", Status: model.StatusGreen,
		State: model.RepoState{Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-9 * time.Hour)}}
	m = update(t, m, repoRefreshedMsg{name: "repo-b", row: older})

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
// staleness reorders the list underneath it (e.g. after a background refresh).
func TestDetailStaysAnchoredAcrossReorder(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-5 * time.Hour)},
		"repo-b": {Name: "repo-b", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Hour)},
	})
	m := newTestModel(t, a)
	m.sortMode = sortStale // repo-a (oldest) is first
	m = update(t, m, press("enter"))
	if m.detailName != "repo-a" {
		t.Fatalf("opened detail on %q, want repo-a", m.detailName)
	}
	// repo-a gets a fresher snapshot than repo-b; under staleness sort repo-b would
	// now sort first, so a cursor-based detail view would jump to repo-b.
	fresher := app.RepoStatus{Name: "repo-a", Status: model.StatusGreen,
		State: model.RepoState{Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Minute)}}
	m = update(t, m, repoRefreshedMsg{name: "repo-a", row: fresher})
	if row, ok := m.detailRow(); !ok || row.Name != "repo-a" {
		t.Errorf("detail jumped to %q after reorder, want repo-a", row.Name)
	}
}

func TestTruncate(t *testing.T) {
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
		{"héllo", 3, "hé…"},    // counts runes, not bytes
		{"", 5, ""},            // empty input stays empty
	}
	for _, tt := range tests {
		if got := truncate(tt.s, tt.max); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
	}
}
