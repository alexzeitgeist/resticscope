package tui

import (
	"context"
	"errors"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
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
	mu        sync.Mutex
	indexed   map[string]bool
	nodes     map[string][]model.BrowseNode
	listCalls map[string]int // per-dir ListDir count, to prove the cache short-circuits requeries
	searchErr error          // when set, Search returns it (to exercise the path-free error path)
	searchN   int            // number of Search calls (to prove supersede / no-scan behaviour)
}

func newFakeBrowseStore() *fakeBrowseStore {
	return &fakeBrowseStore{indexed: map[string]bool{}, nodes: map[string][]model.BrowseNode{}, listCalls: map[string]int{}}
}

// listCount reports how many times ListDir was called for a directory.
func (s *fakeBrowseStore) listCount(dir string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls[dir]
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
	s.listCalls[model.CleanBrowsePath(dir)]++
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

// Search mirrors browsedb.Search's contract using the shared model scorer: it
// matches every indexed node by name, ranks with model.RankFuzzy, caps Rows to
// limit, and reports Total as the full match count before the cap. It never
// returns a path in its error (searchErr, if set, is a plain canned error), so the
// TUI's path-free handling can be exercised without leaking a filename.
func (s *fakeBrowseStore) Search(_ context.Context, repo, snap, query string, limit int) (model.BrowseSearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchN++
	if s.searchErr != nil {
		return model.BrowseSearchResult{}, s.searchErr
	}
	key := browseKey(repo, snap)
	if !s.indexed[key] {
		return model.BrowseSearchResult{}, nil
	}
	var entries []model.BrowseEntry
	for _, n := range s.nodes[key] {
		p := model.CleanBrowsePath(n.Path)
		if p == "/" {
			continue
		}
		entries = append(entries, model.BrowseEntry{
			Path: p, Name: model.BrowseName(n.Name, p), Type: n.Type, LinkTarget: n.LinkTarget,
			IsDir: n.IsDir, Size: n.Size, ModTime: n.ModTime, Permissions: n.Permissions,
			UID: n.UID, GID: n.GID, OwnerKnown: n.OwnerKnown,
		})
	}
	total := len(model.RankFuzzy(entries, query, 0))
	rows := model.RankFuzzy(entries, query, limit)
	return model.BrowseSearchResult{Rows: rows, Total: total}, nil
}

func (s *fakeBrowseStore) searchCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searchN
}

// failSearches makes every subsequent Search return err, so a test can exercise
// the TUI's path-free error handling.
func (s *fakeBrowseStore) failSearches(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchErr = err
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
	a, _ := browseAppWithStore(t, nodes...)
	return a
}

// browseAppWithStore is browseApp but also returns the backing in-memory store so
// a test can assert how many times ListDir was actually called (cache behaviour).
func browseAppWithStore(t *testing.T, nodes ...model.BrowseNode) (*app.App, *fakeBrowseStore) {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	a.Restic = stubRestic{browseNodes: nodes}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func() (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a, store
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

// A visited directory is served from the in-session listing cache: returning to it
// dispatches no async query (so navigation is never paused) and the store is not
// asked for it again. The snapshot is immutable, so the cached rows are still
// correct. This holds for both back/parent and forward re-entry.
func TestBrowseRevisitServedFromListingCache(t *testing.T) {
	a, store := browseAppWithStore(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/b/inner.txt", "inner.txt", false, 1),
	)
	m := openBrowse(t, newTestModel(t, a))
	if store.listCount("/") != 1 {
		t.Fatalf("precondition: root listed %d times after opening, want 1", store.listCount("/"))
	}

	m = pressBrowse(t, m, "j")     // cursor onto /b (index 1 of a,b)
	m = pressBrowse(t, m, "enter") // descend into /b (cache miss -> exactly one query)
	if m.browseDir != "/b" || store.listCount("/b") != 1 {
		t.Fatalf("precondition: dir=%q ListDir(/b)=%d, want /b and 1", m.browseDir, store.listCount("/b"))
	}

	// Back to /: served from cache, so no command is emitted, the model never enters
	// the loading state, and the store is not re-queried for /.
	next, cmd := m.Update(press("backspace"))
	m = next.(Model)
	if cmd != nil {
		t.Error("returning to a visited directory must be served synchronously, with no query command")
	}
	if m.browseLoading {
		t.Error("a cache-served listing must not enter the loading state")
	}
	if m.browseDir != "/" {
		t.Errorf("browseDir = %q, want / after parent", m.browseDir)
	}
	if store.listCount("/") != 1 {
		t.Errorf("root was re-queried: ListDir(/) called %d times, want 1", store.listCount("/"))
	}
	if m.browseCursor != 1 {
		t.Errorf("parent should restore the cursor onto /b (index 1), got %d", m.browseCursor)
	}

	// Re-descending into /b (cursor is already on it) is likewise cache-served — the
	// optimization is not special to parent navigation.
	next, cmd = m.Update(press("enter"))
	m = next.(Model)
	if cmd != nil {
		t.Error("re-entering a visited directory must be served from cache with no query")
	}
	if m.browseDir != "/b" || store.listCount("/b") != 1 {
		t.Errorf("re-descent re-queried the store: dir=%q ListDir(/b)=%d, want /b and 1", m.browseDir, store.listCount("/b"))
	}
	if len(m.browseRows) != 1 || m.browseRows[0].Name != "inner.txt" {
		t.Errorf("cache-served /b listing = %+v, want inner.txt", m.browseRows)
	}
}

// Leaving browse drops the listing cache (which holds filenames) so nothing
// lingers in the model — the same contract clearBrowse enforces for browseRows
// (non-negotiable #1) — and resets the transient browse sort so a prior session's
// order can't leak into the next one.
func TestBrowseLeavingClearsListingCache(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	)))
	if len(m.browseCache) == 0 {
		t.Fatal("precondition: the root listing should be cached after opening browse")
	}

	m = update(t, m, press("o")) // move off the default sort so the reset is observable
	if m.browseSortMode == browseSortName {
		t.Fatal("precondition: pressing o should leave the default sort")
	}

	m = update(t, m, press("q")) // leave browse
	if m.view != detailView {
		t.Fatalf("q should return to detail, view = %d", m.view)
	}
	if m.browseCache != nil {
		t.Errorf("leaving browse must drop the listing cache so no filenames linger, got %d entries", len(m.browseCache))
	}
	if m.browseSortMode != browseSortName {
		t.Errorf("leaving browse must reset the sort to name, got %q", m.browseSortMode.label())
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
	if m.browseIndexRate != 0 {
		t.Errorf("first progress tick should seed the rate window, got rate %.2f", m.browseIndexRate)
	}
	if cmd == nil {
		t.Error("a live progress tick should re-arm the wait command")
	}

	m.browseRateBaseAt = time.Now().Add(-10 * time.Second)
	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 722000})
	if got := browseIndexRateLabel(m.browseIndexRate); got != "72k/s" {
		t.Errorf("recent progress rate = %q, want 72k/s", got)
	}

	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 1000}) // out of order
	if m.browseIndexN != 722000 {
		t.Errorf("a lower out-of-order tick must not lower the count: got %d, want 722000", m.browseIndexN)
	}

	m, staleCmd := m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen - 1, n: 9999})
	if m.browseIndexN != 722000 {
		t.Errorf("a stale tick must not change the count: got %d, want 722000", m.browseIndexN)
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

	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 720000})
	m.browseIndexRate = 72000

	line := m.browseSummaryLine()
	for _, want := range []string{"indexing", "720000 entries", "72k/s", "esc/back cancels"} {
		if !strings.Contains(line, want) {
			t.Fatalf("browseSummaryLine() = %q, missing %q", line, want)
		}
	}
}

func TestBrowseIndexRateLabel(t *testing.T) {
	tests := []struct {
		rate float64
		want string
	}{
		{rate: 0, want: ""},
		{rate: 72000, want: "72k/s"},
		{rate: 125, want: "125/s"},
		{rate: 1.2, want: "1.2/s"},
	}
	for _, tt := range tests {
		if got := browseIndexRateLabel(tt.rate); got != tt.want {
			t.Errorf("browseIndexRateLabel(%f) = %q, want %q", tt.rate, got, tt.want)
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

func TestBrowseIndexErrorCancelsBrowseContext(t *testing.T) {
	m := newTestModel(t, browseApp(t))
	m.view = browseView
	m.browseGen = 7
	cancelled := false
	m.browseCancel = func() { cancelled = true }

	next, cmd := m.applyBrowseIndexed(browseIndexedMsg{gen: 7, err: errors.New("boom")})
	if cmd != nil {
		t.Fatal("index error should not start another command")
	}
	if !cancelled {
		t.Fatal("index error did not call the active browse cancel func")
	}
	if next.browseCancel != nil {
		t.Fatal("index error should clear the browse cancel func after calling it")
	}
	if next.view != detailView {
		t.Fatalf("view = %d, want detailView", next.view)
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

// Directories now carry a real recursive subtree size, so a directory row renders
// its size with humanize.Bytes rather than the old em-dash — and an empty
// directory (subtree size 0) renders "0 B", not "—". The store supplies the
// rolled-up size; the renderer no longer special-cases dirs.
func TestBrowseRendersDirectorySize(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/big", "big", true, 4096),  // a directory carrying a rolled-up size
		bnode("/empty", "empty", true, 0), // an empty directory rolls up to 0
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	view := stripANSI(m.View().Content)

	bigLine := lineContaining(t, view, "big")
	if want := humanize.Bytes(4096); !strings.Contains(bigLine, want) {
		t.Errorf("directory row must render its size %q\n---\n%s", want, bigLine)
	}
	emptyLine := lineContaining(t, view, "empty")
	if !strings.Contains(emptyLine, "0 B") {
		t.Errorf("an empty directory must render 0 B, not an em-dash\n---\n%s", emptyLine)
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

// The full keybinding overlay documents the browse sort cycle under Browse, so the
// reference agrees with the o sort the compact footer advertises (and does not bury
// sort under List alone).
func TestBrowseHelpDocumentsSort(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))

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
		if e.desc == "cycle sort order" {
			found = true
			if e.keys != keyLabel(m.keys.Sort) {
				t.Errorf("Browse sort row keys = %q, want %q", e.keys, keyLabel(m.keys.Sort))
			}
		}
	}
	if !found {
		t.Error("Browse section missing the 'cycle sort order' row")
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

// --- browse → extract wiring (step 07) ---

// extractTestSnapID is a real 64-hex restic snapshot id, the shape
// PlanExtractPaths requires; the browse `e` dispatch passes m.browseSnapshot
// straight through, so the test snapshot must be well-formed.
const extractTestSnapID = "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"

// extractBrowseModel parks a Model in browseView with a valid [extract] config, a
// real 64-hex snapshot, and the given rows under the cursor — the exact state an
// `e` press needs. It bypasses the full index/list flow (covered by the
// browse-open tests) to isolate the extract dispatch.
func extractBrowseModel(t *testing.T, rows []model.BrowseEntry, cursor int) Model {
	t.Helper()
	a := extractApp(t)
	m := newTestModel(t, a)
	m.view = browseView
	m.browseRepo = "repo-a"
	m.browseSnapshot = extractTestSnapID
	m.browseIndexed = true
	m.browseRows = rows
	m.browseCursor = cursor
	return m
}

// e on a directory row opens the extract sub-model in directory-tree mode, with
// every request field derived from the selection and the snapshot pinned by
// browse. TargetRoot stays empty so the app layer applies the cfg default.
func TestBrowseExtractDirectoryOpensSubModel(t *testing.T) {
	m := extractBrowseModel(t, []model.BrowseEntry{
		{Path: "/etc/nginx", Name: "nginx", Type: "dir", IsDir: true, Size: 4096},
	}, 0)

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e on a directory should open extractView, view = %d", m.view)
	}
	req := m.extract.req
	if req.Mode != app.ExtractDirectoryTree {
		t.Errorf("Mode = %v, want ExtractDirectoryTree", req.Mode)
	}
	if req.WasRegularFile {
		t.Error("a directory must not be flagged WasRegularFile")
	}
	if want := model.CleanBrowsePath("/etc/nginx"); req.Source != want {
		t.Errorf("Source = %q, want %q", req.Source, want)
	}
	if req.SourceName != "nginx" {
		t.Errorf("SourceName = %q, want nginx", req.SourceName)
	}
	if req.TargetRoot != "" {
		t.Errorf("TargetRoot = %q, want empty (cfg default)", req.TargetRoot)
	}
	if req.Repo != "repo-a" {
		t.Errorf("Repo = %q, want repo-a", req.Repo)
	}
	if req.SnapshotID != extractTestSnapID {
		t.Errorf("SnapshotID = %q, want %q", req.SnapshotID, extractTestSnapID)
	}
	if req.SnapshotShort != extractTestSnapID[:8] {
		t.Errorf("SnapshotShort = %q, want %q", req.SnapshotShort, extractTestSnapID[:8])
	}
	if m.extract.srcSize != 4096 {
		t.Errorf("srcSize = %d, want 4096 (carried from BrowseEntry.Size)", m.extract.srcSize)
	}
}

// e on a regular-file row opens the sub-model in file-bytes mode with
// WasRegularFile set, so the app layer routes restic dump rather than restore.
func TestBrowseExtractFileOpensSubModel(t *testing.T) {
	m := extractBrowseModel(t, []model.BrowseEntry{
		{Path: "/etc/hosts", Name: "hosts", Type: "file", Size: 412},
	}, 0)

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e on a file should open extractView, view = %d", m.view)
	}
	req := m.extract.req
	if req.Mode != app.ExtractFileBytes {
		t.Errorf("Mode = %v, want ExtractFileBytes", req.Mode)
	}
	if !req.WasRegularFile {
		t.Error("a regular file must set WasRegularFile")
	}
	if req.SourceName != "hosts" {
		t.Errorf("SourceName = %q, want hosts", req.SourceName)
	}
	if m.extract.srcSize != 412 {
		t.Errorf("srcSize = %d, want 412", m.extract.srcSize)
	}
}

// e on a symlink / device / fifo / socket row is rejected: a path-free
// status-line notice, no view change, and no sub-model constructed. Browse's
// listing is children-only, so the snapshot-root path is never reachable here
// (00-framework.md §22) and needs no separate guard.
func TestBrowseExtractRejectsUnsupportedTypes(t *testing.T) {
	for _, typ := range []string{"symlink", "dev", "char", "fifo", "socket"} {
		t.Run(typ, func(t *testing.T) {
			m := extractBrowseModel(t, []model.BrowseEntry{
				{Path: "/dev/thing", Name: "thing", Type: typ},
			}, 0)

			m = update(t, m, press("e"))

			if m.view != browseView {
				t.Errorf("e on a %q entry should stay in browse, view = %d", typ, m.view)
			}
			if !strings.Contains(m.browseNotice, "not supported") {
				t.Errorf("expected an unsupported-type notice, got %q", m.browseNotice)
			}
			if m.extract.req.Source != "" {
				t.Errorf("no sub-model should be built on rejection; req = %+v", m.extract.req)
			}
			// Privacy: the rejection notice must never echo the entry path.
			if strings.Contains(m.browseNotice, "thing") {
				t.Errorf("rejection notice leaked the entry name/path: %q", m.browseNotice)
			}
		})
	}
}

// Quitting the program while an extract is in flight cancels it: the sub-model's
// per-op context is a child of the program op-context (m.ctx) the `e` dispatch
// passes as parentCtx, so a hard quit cascades the cancel and the worker
// unblocks. This is the same cascade the browse-index quit test asserts.
func TestBrowseExtractQuitCancelsInFlight(t *testing.T) {
	m := extractBrowseModel(t, []model.BrowseEntry{
		{Path: "/etc/hosts", Name: "hosts", Type: "file", Size: 1},
	}, 0)
	m = update(t, m, press("e")) // builds the sub-model with parentCtx = m.ctx
	if m.view != extractView {
		t.Fatalf("precondition: e should open extractView, view = %d", m.view)
	}

	// Swap in a blocking driver so the live run hangs until the context fires.
	drv := &fakeExtractDriver{}
	block := make(chan struct{})
	defer close(block)
	drv.push(extractResp{blockOn: block, err: context.Canceled})
	m.extract.drv = drv

	// File source: enter → preview (no restic call), g → running (starts Extract).
	m = update(t, m, press("enter"))
	if m.extract.state != extractStatePreview {
		t.Fatalf("precondition: enter on file review should reach preview, state = %v", m.extract.state)
	}
	next, cmd := m.Update(press("g"))
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("precondition: g should start the run, state = %v", m.extract.state)
	}

	// Run the worker leaf in a goroutine; it blocks in Extract until the per-op
	// context cancels. The progress pump leaf returns nil once the run closes its
	// channel, so we filter for the worker's done message only.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		c := c
		go func() {
			if msg, ok := c().(extractRunDoneMsg); ok {
				done <- msg
			}
		}()
	}

	// Hard quit cancels m.ctx, which the extract's per-op child context inherits.
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("extract did not unblock after quit cancelled the context")
	}
}

// The browse footer advertises the extract action so it is discoverable in
// context. Rendered wide so the full short-help line is visible.
func TestBrowseFooterAdvertisesExtract(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "e extract") {
		t.Errorf("browse footer should advertise 'e extract'\n---\n%s", footer)
	}
}

// The extract action is bound to the single key `e`.
func TestExtractKeyBoundToE(t *testing.T) {
	keys := defaultKeys().Extract.Keys()
	if len(keys) != 1 || keys[0] != "e" {
		t.Errorf("Extract binding keys = %v, want [e]", keys)
	}
}

// --- browse sort ---

// sortBrowseApp builds a browse app whose root directory holds one dir plus three
// files whose name order (a→b→c) disagrees with both their size order and their
// mtime order, so each browse sort mode produces a provably distinct permutation.
func sortBrowseApp(t *testing.T) *app.App {
	t.Helper()
	mid := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	return browseApp(t,
		model.BrowseNode{Path: "/dir", Name: "dir", IsDir: true},
		model.BrowseNode{Path: "/a.txt", Name: "a.txt", Size: 100, ModTime: mid},
		model.BrowseNode{Path: "/b.txt", Name: "b.txt", Size: 300, ModTime: old},
		model.BrowseNode{Path: "/c.txt", Name: "c.txt", Size: 200, ModTime: newt},
	)
}

// Pressing o cycles the directory listing name → size → modified → name, reordering
// browseRows each step, and keeps the cursor on the same entry across the reorder.
func TestBrowseSortCyclesAndPreservesCursor(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))

	nameOrder := []string{"/dir", "/a.txt", "/b.txt", "/c.txt"}
	sizeOrder := []string{"/dir", "/b.txt", "/c.txt", "/a.txt"} // dirs first, then largest
	modOrder := []string{"/dir", "/c.txt", "/a.txt", "/b.txt"}  // dirs first, then newest
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, nameOrder) {
		t.Fatalf("initial listing = %v, want canonical %v", got, nameOrder)
	}

	m = update(t, m, press("j")) // cursor onto /a.txt (index 1)
	if sel := m.selectedBrowseEntry(); sel == nil || sel.Path != "/a.txt" {
		t.Fatalf("precondition: cursor should be on /a.txt, got %+v", sel)
	}

	steps := []struct {
		mode browseSortMode
		want []string
	}{
		{browseSortSize, sizeOrder},
		{browseSortModified, modOrder},
		{browseSortName, nameOrder},
	}
	for _, step := range steps {
		m = update(t, m, press("o"))
		if m.browseSortMode != step.mode {
			t.Fatalf("after o: mode = %q, want %q", m.browseSortMode.label(), step.mode.label())
		}
		if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, step.want) {
			t.Errorf("%s listing = %v, want %v", step.mode.label(), got, step.want)
		}
		if sel := m.selectedBrowseEntry(); sel == nil || sel.Path != "/a.txt" {
			t.Errorf("%s should keep the cursor on /a.txt, got %+v", step.mode.label(), sel)
		}
	}
}

// o is part of the idle-only switch, below the loading guard, so it can't cycle the
// sort mid-index/mid-load (the rows are about to be replaced).
func TestBrowseSortIgnoredWhileLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, press("o")) // size
	if m.browseSortMode != browseSortSize {
		t.Fatalf("precondition: first o should select size, got %q", m.browseSortMode.label())
	}
	frozen := browseRowPaths(m.browseRows)

	m.browseLoading = true
	m = update(t, m, press("o")) // ignored while loading
	if m.browseSortMode != browseSortSize {
		t.Errorf("o while loading should not advance the sort, got %q", m.browseSortMode.label())
	}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, frozen) {
		t.Errorf("o while loading should not reorder rows: got %v want %v", got, frozen)
	}
}

// The active sort persists across navigation: descending into a subdirectory lists
// its children under the same sort, not back at canonical name order.
func TestBrowseSortInheritedIntoSubdir(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/dir", "dir", true, 0),
		bnode("/dir/a.txt", "a.txt", false, 100),
		bnode("/dir/z.bin", "z.bin", false, 300),
	)))
	m = update(t, m, press("o")) // size
	if m.browseSortMode != browseSortSize {
		t.Fatalf("precondition: o should select size, got %q", m.browseSortMode.label())
	}

	m = pressBrowse(t, m, "enter") // descend into /dir (cursor is on it)
	if m.browseDir != "/dir" {
		t.Fatalf("should have descended into /dir, browseDir = %q", m.browseDir)
	}
	if m.browseSortMode != browseSortSize {
		t.Errorf("descending should keep the active sort, got %q", m.browseSortMode.label())
	}
	want := []string{"/dir/z.bin", "/dir/a.txt"} // largest first, not name order
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, want) {
		t.Errorf("subdir listing = %v, want size order %v", got, want)
	}
}

// Sorting only ever reorders a copy: browseCache keeps the canonical ListDir order
// in every mode (so re-cycling never compounds a previous sort), name mode lands
// back on canonical order, and a cached directory is never re-queried.
func TestBrowseSortKeepsCacheCanonical(t *testing.T) {
	a, store := browseAppWithStore(t,
		model.BrowseNode{Path: "/dir", Name: "dir", IsDir: true},
		model.BrowseNode{Path: "/dir/inner.txt", Name: "inner.txt", Size: 1},
		model.BrowseNode{Path: "/a.txt", Name: "a.txt", Size: 100},
		model.BrowseNode{Path: "/b.txt", Name: "b.txt", Size: 300},
		model.BrowseNode{Path: "/c.txt", Name: "c.txt", Size: 200},
	)
	m := openBrowse(t, newTestModel(t, a))
	canonical := browseRowPaths(m.browseCache["/"])

	// size and modified each display a distinct backing array and never touch the cache.
	for _, want := range []browseSortMode{browseSortSize, browseSortModified} {
		m = update(t, m, press("o"))
		if m.browseSortMode != want {
			t.Fatalf("cycle landed on %q, want %q", m.browseSortMode.label(), want.label())
		}
		if &m.browseRows[0] == &m.browseCache["/"][0] {
			t.Errorf("%s must display a copy, not alias the canonical cache", want.label())
		}
		if got := browseRowPaths(m.browseCache["/"]); !reflect.DeepEqual(got, canonical) {
			t.Errorf("%s mutated the cache: %v want canonical %v", want.label(), got, canonical)
		}
	}

	m = update(t, m, press("o")) // back to name
	if m.browseSortMode != browseSortName {
		t.Fatalf("three cycles should return to name, got %q", m.browseSortMode.label())
	}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, canonical) {
		t.Errorf("name mode should restore canonical order: %v want %v", got, canonical)
	}

	// Navigating away and back is still served from the listing cache — sorting did
	// not invalidate it — so the store is not re-queried for a visited directory.
	m = pressBrowse(t, m, "enter")     // into /dir (cursor on it in name mode)
	m = pressBrowse(t, m, "backspace") // back to /
	if m.browseDir != "/" {
		t.Fatalf("should be back at /, browseDir = %q", m.browseDir)
	}
	if store.listCount("/") != 1 {
		t.Errorf("/ was re-queried after sorting+navigation: ListDir(/) = %d, want 1", store.listCount("/"))
	}
}

// While a search is suspended (the user jumped to a match with Enter), o sorts the
// visible directory listing only; the parked fuzzy-ranked browseSearchRows are never
// reordered.
func TestBrowseSortLeavesSuspendedSearchUntouched(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a-report.txt", "a-report.txt", false, 100),
		bnode("/home/z-report.bin", "z-report.bin", false, 300),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")
	if len(m.browseSearchRows) != 2 {
		t.Fatalf("precondition: 'report' should match 2 files, got %d", len(m.browseSearchRows))
	}

	// Enter suspends the search and jumps to the match's parent directory (/home).
	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter on a match should kick off a directory list")
	}
	msg, ok := cmd().(browseDirMsg)
	if !ok {
		t.Fatalf("enter produced %T, want browseDirMsg", cmd())
	}
	m = update(t, m, msg)
	if !m.browseSearchSuspended || m.browseDir != "/home" {
		t.Fatalf("precondition: search suspended over /home, got suspended=%v dir=%q", m.browseSearchSuspended, m.browseDir)
	}
	searchBefore := browseRowPaths(m.browseSearchRows)

	m = update(t, m, press("o")) // sort the visible /home listing
	if m.browseSortMode != browseSortSize {
		t.Errorf("o should sort the directory listing while a search is suspended, mode = %q", m.browseSortMode.label())
	}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, []string{"/home/z-report.bin", "/home/a-report.txt"}) {
		t.Errorf("o should reorder the directory listing, got %v", got)
	}
	if got := browseRowPaths(m.browseSearchRows); !reflect.DeepEqual(got, searchBefore) {
		t.Errorf("o must not reorder the parked search results: got %v want %v", got, searchBefore)
	}
}

// The summary line shows the active non-default sort and omits the indicator in
// name mode, mirroring the list view's header.
func TestBrowseSortSummaryIndicator(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))

	if line := m.browseSummaryLine(); strings.Contains(line, "sort:") {
		t.Errorf("name mode should omit the sort indicator, got %q", line)
	}

	m = update(t, m, press("o")) // size
	if line := m.browseSummaryLine(); !strings.Contains(line, "sort: size") {
		t.Errorf("summary should show the size sort, got %q", line)
	}

	m = update(t, m, press("o")) // modified
	if line := m.browseSummaryLine(); !strings.Contains(line, "sort: modified") {
		t.Errorf("summary should show the modified sort, got %q", line)
	}

	m = update(t, m, press("o")) // back to name
	if line := m.browseSummaryLine(); strings.Contains(line, "sort:") {
		t.Errorf("cycling back to name should drop the sort indicator, got %q", line)
	}
}

// The browse footer advertises the sort key so the cycle is discoverable.
func TestBrowseFooterAdvertisesSort(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "sort") {
		t.Errorf("browse footer should advertise the sort key\n---\n%s", footer)
	}
}

// The directory header marks the active sort column with a single down arrow that
// follows the cycle: "Name ↓" in name mode, "↓ Size" in size mode (leading, because
// Size is right-aligned), "Modified ↓" in modified mode — and only one column is
// ever marked. The arrow lands on each column's padding side so the label never
// shifts. The header line is located by the Owner label, which appears only in the
// table header.
func TestBrowseSortHeaderArrow(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})

	header := func() string { return lineContaining(t, stripANSI(m.View().Content), "Owner") }

	// name mode (default): down arrow on Name only.
	hdr := header()
	if !strings.Contains(hdr, "Name ↓") {
		t.Errorf("name sort should mark Name with ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "↓ Size") || strings.Contains(hdr, "Modified ↓") {
		t.Errorf("name sort should not mark Size/Modified\n%q", hdr)
	}

	// size mode: the arrow moves to Size, leading it (Size is right-aligned, so the
	// arrow sits to the left to keep the label pinned over the numbers).
	m = update(t, m, press("o"))
	hdr = header()
	if !strings.Contains(hdr, "↓ Size") {
		t.Errorf("size sort should mark Size with a leading ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "Name ↓") || strings.Contains(hdr, "Modified ↓") {
		t.Errorf("size sort should mark only Size\n%q", hdr)
	}

	// modified mode: the arrow moves to Modified.
	m = update(t, m, press("o"))
	hdr = header()
	if !strings.Contains(hdr, "Modified ↓") {
		t.Errorf("modified sort should mark Modified with ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "Name ↓") || strings.Contains(hdr, "↓ Size") {
		t.Errorf("modified sort should mark only Modified\n%q", hdr)
	}
}

// The global search results are relevance-ranked, not column-sorted, so their table
// header must carry no sort arrow (even though the directory sort mode persists
// underneath the parked listing).
func TestBrowseSearchHeaderHasNoSortArrow(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	m = update(t, m, press("o")) // a non-default dir sort is active under the search
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	hdr := lineContaining(t, stripANSI(m.View().Content), "Owner")
	if strings.ContainsAny(hdr, "↑↓") {
		t.Errorf("search header must carry no sort arrow\n%q", hdr)
	}
}
