package tui

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// --- browse test fakes ---

func bnode(p, name string, isDir bool, size int64) model.BrowseNode {
	return model.BrowseNode{Path: p, Name: name, IsDir: isDir, Size: size}
}

// fakeBrowseStore is an in-memory app.BrowseStore for TUI state tests. It records
// the nodes streamed during indexing and serves directory listings from them, so
// the browse flow (index → list) runs end to end without real SQLite. ListDir
// returns only direct children, dirs-first then case-insensitive, mirroring
// browsedb's contract (which has its own dedicated tests).
type fakeBrowseStore struct {
	mu      sync.Mutex
	indexed map[string]bool
	nodes   map[string][]model.BrowseNode
}

func newFakeBrowseStore() *fakeBrowseStore {
	return &fakeBrowseStore{indexed: map[string]bool{}, nodes: map[string][]model.BrowseNode{}}
}

func browseKey(repo, snap string) string { return repo + "\x00" + snap }

func (s *fakeBrowseStore) IsIndexed(_ context.Context, repo, snap string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexed[browseKey(repo, snap)], nil
}

func (s *fakeBrowseStore) BeginIndex(_ context.Context, repo, snap string) (app.IndexWriter, error) {
	return &fakeBrowseWriter{store: s, key: browseKey(repo, snap)}, nil
}

func (s *fakeBrowseStore) ListDir(_ context.Context, repo, snap, dir string) ([]model.BrowseEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := browseKey(repo, snap)
	if !s.indexed[key] {
		return nil, nil
	}
	parent := model.CleanBrowsePath(dir)
	var out []model.BrowseEntry
	for _, n := range s.nodes[key] {
		p := model.CleanBrowsePath(n.Path)
		if p == "/" || path.Dir(p) != parent {
			continue
		}
		out = append(out, model.BrowseEntry{
			Path: p, Name: model.BrowseName(n.Name, p), Type: n.Type, LinkTarget: n.LinkTarget,
			IsDir: n.IsDir, Size: n.Size, ModTime: n.ModTime, Permissions: n.Permissions,
			UID: n.UID, GID: n.GID, OwnerKnown: n.OwnerKnown,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // dirs first
		}
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *fakeBrowseStore) Close() error { return nil }

type fakeBrowseWriter struct {
	store *fakeBrowseStore
	key   string
	buf   []model.BrowseNode
	count int
}

func (w *fakeBrowseWriter) Add(_ context.Context, n model.BrowseNode) error {
	w.buf = append(w.buf, n)
	w.count++
	return nil
}

func (w *fakeBrowseWriter) Count() int { return w.count }

func (w *fakeBrowseWriter) Commit(_ context.Context) error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	w.store.nodes[w.key] = w.buf
	w.store.indexed[w.key] = true
	return nil
}

func (w *fakeBrowseWriter) Rollback() error { return nil }

// browseApp builds a detail app whose restic streams the given nodes and whose
// browse session is backed by an in-memory store, so b → index → list runs end to
// end. The store is shared across re-opens within the test so a second browse of
// the same snapshot short-circuits via IsIndexed.
func browseApp(t *testing.T, nodes ...model.BrowseNode) *app.App {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	a.Restic = stubRestic{browseNodes: nodes}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func() (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a
}

// findBrowseIndexed runs the leaves of a browse command and returns the
// browseIndexedMsg. The index command is batched with the progress-wait command;
// the index leaf runs first and produces the message, so the wait leaf (which
// would block on the progress channel) is never reached.
func findBrowseIndexed(t *testing.T, cmd tea.Cmd) browseIndexedMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a browse command")
	}
	for _, c := range leafCmds(t, cmd) {
		if msg, ok := c().(browseIndexedMsg); ok {
			return msg
		}
	}
	t.Fatal("no browseIndexedMsg from the browse command")
	return browseIndexedMsg{}
}

// drivePastIndex delivers a successful browseIndexedMsg and runs the resulting
// directory-list command, so the model lands on the first listing.
func drivePastIndex(t *testing.T, m Model, idx browseIndexedMsg) Model {
	t.Helper()
	next, listCmd := m.Update(idx)
	m = next.(Model)
	if m.view != browseView {
		return m // an index error fell back to detail; nothing to list
	}
	if listCmd == nil {
		t.Fatal("a successful index should kick off the first directory list")
	}
	msg, ok := listCmd().(browseDirMsg)
	if !ok {
		t.Fatalf("list command produced %T, want browseDirMsg", listCmd())
	}
	return update(t, m, msg)
}

// openBrowse drives the model into browseView with the first directory listed:
// enter detail, press b, run the index, then run the first directory list.
func openBrowse(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	return drivePastIndex(t, m, findBrowseIndexed(t, cmd))
}

// pressBrowse presses a browse key and, if it produced a navigation command,
// runs that command and delivers the resulting browseDirMsg. Cursor-only keys
// (which emit no command) and no-op keys are returned unchanged.
func pressBrowse(t *testing.T, m Model, k string) Model {
	t.Helper()
	next, cmd := m.Update(press(k))
	m = next.(Model)
	if cmd == nil {
		return m
	}
	msg, ok := cmd().(browseDirMsg)
	if !ok {
		t.Fatalf("%q produced %T, want browseDirMsg", k, cmd())
	}
	return update(t, m, msg)
}

// --- tests ---

// b indexes the snapshot once, then lists the root directory: the view shows the
// top-level children and the model is marked indexed and idle.
func TestBrowseIndexesAndListsRoot(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/alex", "alex", true, 0),
		bnode("/home/alex/f.txt", "f.txt", false, 42),
	)))

	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if m.browseLoading || !m.browseIndexed {
		t.Errorf("after the first listing: loading=%v indexed=%v, want idle+indexed", m.browseLoading, m.browseIndexed)
	}
	if m.browseDir != "/" {
		t.Errorf("browseDir = %q, want /", m.browseDir)
	}
	if len(m.browseRows) != 1 || m.browseRows[0].Name != "home" {
		t.Fatalf("root listing = %+v, want a single 'home' dir", m.browseRows)
	}
	if !strings.Contains(m.View().Content, "home") {
		t.Errorf("browse view missing the listed directory\n---\n%s", m.View().Content)
	}
}

// Enter descends into the selected directory and resets the cursor to the top of
// the new listing.
func TestBrowseEnterDescendsAndResetsCursor(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/b/child.txt", "child.txt", false, 5),
	)))

	m = pressBrowse(t, m, "j") // cursor onto /b (second root entry)
	if m.browseCursor != 1 {
		t.Fatalf("precondition: cursor = %d, want 1", m.browseCursor)
	}
	m = pressBrowse(t, m, "enter") // descend into /b
	if m.browseDir != "/b" {
		t.Errorf("browseDir = %q, want /b", m.browseDir)
	}
	if m.browseCursor != 0 {
		t.Errorf("descending should reset the cursor to 0, got %d", m.browseCursor)
	}
	if len(m.browseRows) != 1 || m.browseRows[0].Name != "child.txt" {
		t.Errorf("/b listing = %+v, want child.txt", m.browseRows)
	}
}

// Stepping up to the parent restores the cursor onto the child we descended from,
// so enter-then-parent feels like walking a path.
func TestBrowseParentRestoresCursorOntoChild(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/c", "c", true, 0),
		bnode("/b/inner.txt", "inner.txt", false, 1),
	)))

	m = pressBrowse(t, m, "j")     // cursor onto /b (index 1 of a,b,c)
	m = pressBrowse(t, m, "enter") // descend into /b
	if m.browseDir != "/b" {
		t.Fatalf("precondition: browseDir = %q, want /b", m.browseDir)
	}
	m = pressBrowse(t, m, "backspace") // step back up to /
	if m.browseDir != "/" {
		t.Errorf("browseDir = %q, want / after parent", m.browseDir)
	}
	if m.browseCursor != 1 {
		t.Errorf("parent should restore the cursor onto /b (index 1), got %d", m.browseCursor)
	}
}

// Right arrow and l are aliases for Enter: each opens the selected directory; on a
// file they are a no-op (files have no open action).
func TestBrowseRightArrowOpensDirectory(t *testing.T) {
	for _, k := range []string{"right", "l"} {
		t.Run(k, func(t *testing.T) {
			m := openBrowse(t, newTestModel(t, browseApp(t,
				bnode("/dir", "dir", true, 0),
				bnode("/dir/child.txt", "child.txt", false, 5),
			)))
			if m.browseDir != "/" {
				t.Fatalf("precondition: browseDir = %q, want /", m.browseDir)
			}
			m = pressBrowse(t, m, k) // cursor is on /dir
			if m.browseDir != "/dir" {
				t.Errorf("%q should open the selected directory, browseDir = %q", k, m.browseDir)
			}
		})
	}
}

func TestBrowseRightArrowOnFileIsNoOp(t *testing.T) {
	for _, k := range []string{"right", "l"} {
		t.Run(k, func(t *testing.T) {
			m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a.txt", "a.txt", false, 5))))
			m = pressBrowse(t, m, k) // cursor is on the file
			if m.browseDir != "/" {
				t.Errorf("%q on a file should be a no-op, browseDir = %q", k, m.browseDir)
			}
		})
	}
}

// A late browse message whose generation no longer matches (a cancelled or
// superseded navigation) is silently discarded and never resurrects browse state.
func TestBrowseStaleMessagesDropped(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a", "a", true, 0))))
	gen, dir := m.browseGen, m.browseDir
	rowsBefore := len(m.browseRows)

	// A stale directory listing must not replace the current rows or path.
	m = update(t, m, browseDirMsg{
		gen:  gen + 99,
		dir:  "/zzz",
		rows: []model.BrowseEntry{{Path: "/zzz", Name: "zzz", IsDir: true}},
	})
	if m.browseDir != dir || len(m.browseRows) != rowsBefore {
		t.Errorf("stale browseDirMsg applied: dir=%q rows=%d", m.browseDir, len(m.browseRows))
	}
	if strings.Contains(m.View().Content, "zzz") {
		t.Errorf("stale listing leaked into the view\n---\n%s", m.View().Content)
	}

	// A stale index-done message must not flip the view or indexed state.
	m = update(t, m, browseIndexProgressMsg{gen: gen + 99, n: 123})
	if m.browseIndexN == 123 {
		t.Errorf("stale progress tick applied: browseIndexN = %d", m.browseIndexN)
	}
}

// A stale browseIndexedMsg (its generation superseded by a back/cancel) does not
// drive a directory list or flip indexed state.
func TestBrowseStaleIndexedMsgDropped(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b"))
	m = next.(Model)

	next, cmd := m.Update(browseIndexedMsg{gen: m.browseGen - 1, err: nil}) // stale
	m = next.(Model)
	if cmd != nil {
		t.Error("a stale browseIndexedMsg should not start a directory list")
	}
	if m.browseIndexed {
		t.Error("a stale browseIndexedMsg must not mark the snapshot indexed")
	}
}

// Back (q and esc) during indexing cancels the in-flight index, returns to the
// detail view, and clears all session browse state so no filenames linger.
func TestBrowseBackDuringIndexingCancelsAndClears(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		t.Run(k, func(t *testing.T) {
			m := newTestModel(t, browseApp(t, bnode("/secret", "secret", true, 0)))
			m = update(t, m, press("enter"))
			next, _ := m.Update(press("b"))
			m = next.(Model)
			if !m.browseLoading || m.browseIndexed {
				t.Fatal("precondition: should be indexing (loading, not indexed)")
			}
			genBefore := m.browseGen

			next, cmd := m.Update(press(k))
			m = next.(Model)
			if m.view != detailView {
				t.Errorf("%q during indexing should return to detail, view = %d", k, m.view)
			}
			if m.browseRepo != "" || m.browseSnapshot != "" || m.browseDir != "" || m.browseRows != nil {
				t.Errorf("%q should clear session state: repo=%q snap=%q dir=%q rows=%v",
					k, m.browseRepo, m.browseSnapshot, m.browseDir, m.browseRows)
			}
			if m.browseLoading || m.browseIndexed {
				t.Errorf("%q should clear load/indexed flags: loading=%v indexed=%v", k, m.browseLoading, m.browseIndexed)
			}
			if m.browseGen <= genBefore {
				t.Errorf("%q should advance the generation to drop the cancelled index's late msgs (gen=%d)", k, m.browseGen)
			}
			if cmd != nil {
				t.Errorf("%q leaving browse should emit no command", k)
			}
		})
	}
}

// Progress ticks advance the running count monotonically, re-arm the wait, and a
// stale-generation tick is dropped without re-arming.
func TestBrowseProgressCoalescesMonotonically(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b"))
	m = next.(Model)
	gen := m.browseGen

	m, cmd := m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 2000})
	if m.browseIndexN != 2000 {
		t.Errorf("browseIndexN = %d, want 2000", m.browseIndexN)
	}
	if cmd == nil {
		t.Error("a live progress tick should re-arm the wait command")
	}

	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 1000}) // out of order
	if m.browseIndexN != 2000 {
		t.Errorf("a lower out-of-order tick must not lower the count: got %d, want 2000", m.browseIndexN)
	}

	m, staleCmd := m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen - 1, n: 9999})
	if m.browseIndexN != 2000 {
		t.Errorf("a stale tick must not change the count: got %d, want 2000", m.browseIndexN)
	}
	if staleCmd != nil {
		t.Error("a stale tick must not re-arm the wait command")
	}
}

func TestBrowseIndexingSummaryShowsRate(t *testing.T) {
	a := browseApp(t, bnode("/a", "a", true, 0))
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b"))
	m = next.(Model)
	gen := m.browseGen

	m.browseIndexAt = time.Now().Add(-10 * time.Second)
	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 720000})

	line := m.browseSummaryLine()
	for _, want := range []string{"indexing", "720000 entries", "72k/s", "esc/back cancels"} {
		if !strings.Contains(line, want) {
			t.Fatalf("browseSummaryLine() = %q, missing %q", line, want)
		}
	}
}

func TestBrowseIndexRateLabel(t *testing.T) {
	tests := []struct {
		entries int
		elapsed time.Duration
		want    string
	}{
		{entries: 0, elapsed: 10 * time.Second, want: ""},
		{entries: 1000, elapsed: 500 * time.Millisecond, want: ""},
		{entries: 720000, elapsed: 10 * time.Second, want: "72k/s"},
		{entries: 1250, elapsed: 10 * time.Second, want: "125/s"},
		{entries: 12, elapsed: 10 * time.Second, want: "1.2/s"},
	}
	for _, tt := range tests {
		if got := browseIndexRateLabel(tt.entries, tt.elapsed); got != tt.want {
			t.Errorf("browseIndexRateLabel(%d, %v) = %q, want %q", tt.entries, tt.elapsed, got, tt.want)
		}
	}
}

// waitForIndexProgress turns a buffered value into a progress message and a closed
// channel into a nil message that ends the wait loop.
func TestWaitForIndexProgress(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 7
	if msg := waitForIndexProgress(3, ch)(); msg != (browseIndexProgressMsg{gen: 3, n: 7}) {
		t.Errorf("waitForIndexProgress value = %#v, want gen 3 n 7", msg)
	}

	closed := make(chan int)
	close(closed)
	if msg := waitForIndexProgress(3, closed)(); msg != nil {
		t.Errorf("a closed channel should end the wait with a nil msg, got %#v", msg)
	}
}

// An index error has no listing to show: the redacted message is surfaced and the
// model falls back to the detail view with session state cleared.
func TestBrowseIndexErrorReturnsToDetail(t *testing.T) {
	a := browseApp(t)
	a.Restic = stubRestic{browseErr: errors.New("repository is locked")}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)

	next, _ = m.Update(findBrowseIndexed(t, cmd))
	m = next.(Model)
	if m.view != detailView {
		t.Errorf("an index error should fall back to detail, view = %d", m.view)
	}
	if !strings.Contains(m.statusMsg, "browse:") {
		t.Errorf("statusMsg = %q, want a browse error notice", m.statusMsg)
	}
	if m.browseRows != nil || m.browseRepo != "" {
		t.Errorf("an index error should clear session state: rows=%v repo=%q", m.browseRows, m.browseRepo)
	}
}

// While the one-time index runs, the view must keep a visible cancel affordance:
// a minutes-long crawl on a huge snapshot must never look hung. This is
// non-negotiable.
func TestBrowseIndexingShowsCancelAffordance(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b")) // start indexing; do not run the index command
	m = next.(Model)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	if !m.browseLoading || m.browseIndexed {
		t.Fatal("precondition: should be mid-index (loading, not indexed)")
	}
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "indexing") {
		t.Errorf("indexing view should report progress\n---\n%s", view)
	}
	if !strings.Contains(view, "esc/back cancels") {
		t.Errorf("indexing view must keep a visible cancel affordance\n---\n%s", view)
	}
}

// Leaving browse clears the UI rows but not the session DB: reopening the same
// snapshot in the same run short-circuits via IsIndexed and never re-streams from
// restic (proven by swapping in a restic that would error if streamed).
func TestBrowseReopenSkipsRestic(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/secret-dir", "secret-dir", true, 0)))
	m = openBrowse(t, m)
	if !strings.Contains(m.View().Content, "secret-dir") {
		t.Fatal("precondition: first browse should list the indexed entry")
	}

	m = update(t, m, press("q")) // back to detail; clearBrowse drops UI rows
	if m.view != detailView || m.browseRows != nil {
		t.Fatalf("leaving browse should return to detail and clear rows: view=%d rows=%v", m.view, m.browseRows)
	}

	// A re-stream now would fail; a correct reopen must short-circuit via IsIndexed.
	m.app.Restic = stubRestic{browseErr: errors.New("must not re-stream an indexed snapshot")}
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	m = drivePastIndex(t, m, findBrowseIndexed(t, cmd))

	if m.view != browseView {
		t.Errorf("reopening an indexed snapshot should succeed without restic, view = %d", m.view)
	}
	if !strings.Contains(m.View().Content, "secret-dir") {
		t.Errorf("reopened browse should list the still-indexed entry\n---\n%s", m.View().Content)
	}
}

// Quitting (ctrl+c) while the one-time index is in flight cancels the browse
// context so the restic subprocess does not outlive the UI.
func TestBrowseQuitCancelsInFlightIndex(t *testing.T) {
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(time.Minute)}
	started := make(chan struct{})
	a.Restic = blockingRestic{started: started}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func() (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	m := newTestModel(t, a)

	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected a browse command")
	}
	// Run every leaf; the index leaf blocks in restic, the wait leaf blocks on the
	// progress channel. Forward only the index result so the assertion below is not
	// satisfied by the wait leaf.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		c := c
		go func() {
			if msg, ok := c().(browseIndexedMsg); ok {
				done <- msg
			}
		}()
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("index never reached restic")
	}

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // hard quit cancels m.ctx -> browse ctx

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("index did not unblock after quit cancelled the context")
	}
}

// lineContaining returns the first line of s that contains sub, failing the test
// if none does. It lets owner/perms assertions target a specific table row rather
// than the whole view, so a value rendered on one row can't accidentally satisfy
// an assertion meant for another.
func lineContaining(t *testing.T, s, sub string) string {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", sub, s)
	return ""
}

// At a wide width the browse table shows every metadata column (Modified, Perms,
// Owner) with their values; narrowing the terminal drops Owner, then Perms, then
// Modified in that priority order while Name and Size always remain.
func TestBrowseRendersMetadataColumnsResponsively(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)))

	// Wide: every promoted column header and value is present.
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	wide := stripANSI(m.View().Content)
	for _, want := range []string{"Modified", "Perms", "Owner", "2026-05-26 11:28", "-rw-r--r--", "1000:1000"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide browse view missing %q\n---\n%s", want, wide)
		}
	}

	// Narrow (width 50): only Modified survives the promotion budget, so the Perms
	// and Owner columns and their values drop, while Name and Size stay.
	m = update(t, m, tea.WindowSizeMsg{Width: 50, Height: 40})
	narrow := stripANSI(m.View().Content)
	if strings.Contains(narrow, "Owner") || strings.Contains(narrow, "1000:1000") {
		t.Errorf("narrow browse view should drop the Owner column\n---\n%s", narrow)
	}
	if strings.Contains(narrow, "Perms") || strings.Contains(narrow, "-rw-r--r--") {
		t.Errorf("narrow browse view should drop the Perms column\n---\n%s", narrow)
	}
	if !strings.Contains(narrow, "Name") || !strings.Contains(narrow, "file.txt") {
		t.Errorf("narrow browse view must keep the Name column\n---\n%s", narrow)
	}
	if !strings.Contains(narrow, "Modified") {
		t.Errorf("width 50 should still promote Modified\n---\n%s", narrow)
	}
}

func TestBrowseTableWidthCapsWideTerminals(t *testing.T) {
	cases := map[int]int{
		80:                      80, // narrower than the cap: full width
		browseTableMaxWidth:     browseTableMaxWidth,
		browseTableMaxWidth + 1: browseTableMaxWidth, // just over: capped
		240:                     browseTableMaxWidth, // far over: capped
	}
	for in, want := range cases {
		if got := browseTableWidth(in); got != want {
			t.Errorf("browseTableWidth(%d) = %d, want %d", in, got, want)
		}
	}
}

// On a very wide terminal the file table is bounded to browseTableMaxWidth rather
// than stretching the Name column the full width, so metadata never drifts to the
// far right. Both the data row and the column header (which belong to the table)
// must stay within the cap.
func TestBrowseTableBoundedOnWideTerminal(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 240, Height: 40})
	view := stripANSI(m.View().Content)

	// View() right-pads every line to the full terminal width as part of its
	// layout block, so trim that trailing padding before measuring: what matters
	// is that the table's content (the metadata) does not drift past the cap.
	row := strings.TrimRight(lineContaining(t, view, "file.txt"), " ")
	if w := lipgloss.Width(row); w > browseTableMaxWidth {
		t.Errorf("file row width = %d, want <= %d (table must be capped on a wide terminal)\n%q", w, browseTableMaxWidth, row)
	}
	hdr := strings.TrimRight(lineContaining(t, view, "Owner"), " ")
	if w := lipgloss.Width(hdr); w > browseTableMaxWidth {
		t.Errorf("header row width = %d, want <= %d\n%q", w, browseTableMaxWidth, hdr)
	}
}

// A filename with a wide rune (here U+FE55, the small colon some apps substitute
// for the filesystem-illegal ':') must not push the metadata columns out of
// alignment: the Name cell is sized by display width, not rune count. Were it
// rune-counted, the wide name would render one cell too wide and the trailing
// Owner value would be clipped (e.g. 10316:1023 → 10316:102), the exact symptom
// reported. Assert both the wide-rune and plain rows keep their full owner.
func TestBrowseWideRuneNameKeepsColumnsAligned(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/wide﹕name.txt", Name: "wide﹕name.txt", Size: 42, ModTime: mod, Permissions: "-rw-rw----", UID: 10316, GID: 1023, OwnerKnown: true},
		model.BrowseNode{Path: "/plain.txt", Name: "plain.txt", Size: 42, ModTime: mod, Permissions: "-rw-rw----", UID: 10316, GID: 1023, OwnerKnown: true},
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	view := stripANSI(m.View().Content)

	for _, name := range []string{"wide", "plain"} {
		row := lineContaining(t, view, name)
		if !strings.Contains(row, "10316:1023") {
			t.Errorf("%s row lost owner alignment (want full 10316:1023)\n%q", name, row)
		}
	}
}

// A node carrying explicit uid=0,gid=0 renders as a real "0:0" owner, while a node
// that omitted uid/gid renders the missing-owner em-dash — never a spurious 0:0.
// Both rows carry a non-zero mtime so the Modified column is never an em-dash;
// the assertions then target each row's own line, so the only em-dash on the anon
// line comes from its missing owner cell (and a regression to 0:0 would fail it).
func TestBrowseOwnerRendersRootVersusMissing(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/root.txt", Name: "root.txt", Size: 1, ModTime: mod, Permissions: "-rw-------", UID: 0, GID: 0, OwnerKnown: true},
		model.BrowseNode{Path: "/anon.txt", Name: "anon.txt", Size: 1, ModTime: mod, Permissions: "-rw-r--r--"},
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	view := stripANSI(m.View().Content)

	rootLine := lineContaining(t, view, "root.txt")
	if !strings.Contains(rootLine, "0:0") {
		t.Errorf("a real root-owned node must render 0:0\n---\n%s", rootLine)
	}

	anonLine := lineContaining(t, view, "anon.txt")
	if strings.Contains(anonLine, "0:0") {
		t.Errorf("a node with missing owner metadata must not render 0:0\n---\n%s", anonLine)
	}
	if !strings.Contains(anonLine, "—") {
		t.Errorf("a node with missing owner metadata must render an em-dash owner\n---\n%s", anonLine)
	}
}

// The help overlay's Browse section documents the open alias as enter/→/l, while
// the compact browse footer must not advertise the right-arrow alias.
func TestBrowseHelpDocumentsOpenAliasFooterHidesIt(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))

	if footer := stripANSI(m.footerView()); strings.Contains(footer, "→") {
		t.Errorf("browse footer must not advertise the right-arrow open alias\n---\n%s", footer)
	}

	_, right := m.helpColumns()
	var browse helpSection
	for _, s := range right {
		if s.title == "Browse" {
			browse = s
		}
	}
	if browse.title == "" {
		t.Fatal("help overlay missing Browse section")
	}
	found := false
	for _, e := range browse.entries {
		if e.desc == "open directory" {
			found = true
			if e.keys != "enter/→/l" {
				t.Errorf("Browse open row keys = %q, want enter/→/l", e.keys)
			}
		}
	}
	if !found {
		t.Error("Browse section missing the 'open directory' row")
	}
}

// The browse footer advertises navigation, open, shell, and back — and never the
// removed load-more affordance.
func TestBrowseFooterHasNoLoadMore(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))
	footer := stripANSI(m.footerView())
	if strings.Contains(footer, "load more") {
		t.Errorf("browse footer must not advertise load more\n---\n%s", footer)
	}
	if !strings.Contains(footer, "shell") || !strings.Contains(footer, "back") {
		t.Errorf("browse footer should show shell/back guidance\n---\n%s", footer)
	}
}

// The detail-view footer reads "b browse" (shortened from "browse files").
func TestDetailFooterBrowseWording(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter")) // enter detail view
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "browse") {
		t.Errorf("detail footer should advertise browse\n---\n%s", footer)
	}
	if strings.Contains(footer, "browse files") {
		t.Errorf("detail footer should read 'b browse', not 'browse files'\n---\n%s", footer)
	}
}
