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

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func bnode(p, name string, isDir bool, size int64) model.BrowseNode {
	// Mirror restic's always-present node type for production-equivalent gating.
	t := "file"
	if isDir {
		t = "dir"
	}
	return model.BrowseNode{Path: p, Name: name, Type: t, IsDir: isDir, Size: size}
}

// fakeBrowseStore runs index, list, and search flows in memory. Its direct-child,
// directory-first ordering mirrors browsedb's separately tested contract.
type fakeBrowseStore struct {
	mu        sync.Mutex
	indexed   map[string]bool
	nodes     map[string][]model.BrowseNode
	listCalls map[string]int
	searchErr error
	searchN   int
}

func newFakeBrowseStore() *fakeBrowseStore {
	return &fakeBrowseStore{indexed: map[string]bool{}, nodes: map[string][]model.BrowseNode{}, listCalls: map[string]int{}}
}

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

// SubtreeCounts mirrors recursive store counts, excluding dir itself and
// reporting uncommitted snapshots as unknown.
func (s *fakeBrowseStore) SubtreeCounts(_ context.Context, repo, snap, dir string) (files, dirs int, known bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := browseKey(repo, snap)
	if !s.indexed[key] {
		return 0, 0, false, nil
	}
	root := model.CleanBrowsePath(dir)
	prefix := root
	if prefix != "/" {
		prefix += "/"
	}
	for _, n := range s.nodes[key] {
		p := model.CleanBrowsePath(n.Path)
		if p == root || !strings.HasPrefix(p, prefix) {
			continue
		}
		switch {
		case n.IsDir:
			dirs++
		case n.Type == "file":
			files++
		}
	}
	return files, dirs, true, nil
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
			return out[i].IsDir
		}
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Search uses the shared scorer, caps Rows, and reports the uncapped Total. Its
// canned errors contain no paths.
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

// browseApp builds an in-memory index-to-list flow whose store persists across
// reopenings within the test.
func browseApp(t *testing.T, nodes ...model.BrowseNode) *app.App {
	t.Helper()
	a, _ := browseAppWithStore(t, nodes...)
	return a
}

// browseAppWithStore also exposes the store for call-count assertions.
func browseAppWithStore(t *testing.T, nodes ...model.BrowseNode) (*app.App, *fakeBrowseStore) {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	a.Restic = stubRestic{browseNodes: nodes}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func(context.Context) (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a, store
}

// findBrowseIndexed runs command leaves until the index result, before the
// progress-wait leaf can block.
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

// drivePastIndex delivers an index result and the first directory listing.
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

// openBrowse enters browse and completes its index and root listing.
func openBrowse(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	return drivePastIndex(t, m, findBrowseIndexed(t, cmd))
}

// pressBrowse delivers any directory result produced by a browse key.
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

// Opening browse indexes once and lists top-level children.
func TestBrowseIndexesAndListsRoot(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/alex", "alex", true, 0),
		bnode("/home/alex/f.txt", "f.txt", false, 42),
	)))

	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if m.isBrowseLoading || !m.browseIndexed {
		t.Errorf("after the first listing: loading=%v indexed=%v, want idle+indexed", m.isBrowseLoading, m.browseIndexed)
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

// Enter descends and resets the cursor.
func TestBrowseEnterDescendsAndResetsCursor(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/b/child.txt", "child.txt", false, 5),
	)))

	m = pressBrowse(t, m, "j")
	if m.browseCursor != 1 {
		t.Fatalf("precondition: cursor = %d, want 1", m.browseCursor)
	}
	m = pressBrowse(t, m, "enter")
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

// Parent navigation restores the cursor to the child just left.
func TestBrowseParentRestoresCursorOntoChild(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/c", "c", true, 0),
		bnode("/b/inner.txt", "inner.txt", false, 1),
	)))

	m = pressBrowse(t, m, "j")
	m = pressBrowse(t, m, "enter")
	if m.browseDir != "/b" {
		t.Fatalf("precondition: browseDir = %q, want /b", m.browseDir)
	}
	m = pressBrowse(t, m, "backspace")
	if m.browseDir != "/" {
		t.Errorf("browseDir = %q, want / after parent", m.browseDir)
	}
	if m.browseCursor != 1 {
		t.Errorf("parent should restore the cursor onto /b (index 1), got %d", m.browseCursor)
	}
}

// Visited immutable directories are served synchronously from the session cache.
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

	m = pressBrowse(t, m, "j")
	m = pressBrowse(t, m, "enter")
	if m.browseDir != "/b" || store.listCount("/b") != 1 {
		t.Fatalf("precondition: dir=%q ListDir(/b)=%d, want /b and 1", m.browseDir, store.listCount("/b"))
	}

	// A cached parent emits no command and never enters loading state.
	next, cmd := m.Update(press("backspace"))
	m = next.(Model)
	if cmd != nil {
		t.Error("returning to a visited directory must be served synchronously, with no query command")
	}
	if m.isBrowseLoading {
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

// Leaving browse clears filename-bearing caches and transient sort state.
func TestBrowseLeavingClearsListingCache(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	)))
	if len(m.browseCache) == 0 {
		t.Fatal("precondition: the root listing should be cached after opening browse")
	}

	m = update(t, m, press("o"))
	if m.browseSortMode == browseSortName {
		t.Fatal("precondition: pressing o should leave the default sort")
	}

	m = update(t, m, press("q"))
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

// Right arrow and l open directories but ignore files.
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
			m = pressBrowse(t, m, k)
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
			m = pressBrowse(t, m, k)
			if m.browseDir != "/" {
				t.Errorf("%q on a file should be a no-op, browseDir = %q", k, m.browseDir)
			}
		})
	}
}

// Stale generations cannot resurrect browse state.
func TestBrowseStaleMessagesDropped(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a", "a", true, 0))))
	gen, dir := m.browseGen, m.browseDir
	rowsBefore := len(m.browseRows)

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

	m = update(t, m, browseIndexProgressMsg{gen: gen + 99, n: 123})
	if m.browseIndexN == 123 {
		t.Errorf("stale progress tick applied: browseIndexN = %d", m.browseIndexN)
	}
}

// A superseded index result cannot start listing or mark the snapshot indexed.
func TestBrowseStaleIndexedMsgDropped(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b"))
	m = next.(Model)

	next, cmd := m.Update(browseIndexedMsg{gen: m.browseGen - 1, err: nil})
	m = next.(Model)
	if cmd != nil {
		t.Error("a stale browseIndexedMsg should not start a directory list")
	}
	if m.browseIndexed {
		t.Error("a stale browseIndexedMsg must not mark the snapshot indexed")
	}
}

// Back during indexing cancels work and clears browse state.
func TestBrowseBackDuringIndexingCancelsAndClears(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		t.Run(k, func(t *testing.T) {
			m := newTestModel(t, browseApp(t, bnode("/secret", "secret", true, 0)))
			m = update(t, m, press("enter"))
			next, _ := m.Update(press("b"))
			m = next.(Model)
			if !m.isBrowseLoading || m.browseIndexed {
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
			if m.isBrowseLoading || m.browseIndexed {
				t.Errorf("%q should clear load/indexed flags: loading=%v indexed=%v", k, m.isBrowseLoading, m.browseIndexed)
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

// Progress is monotonic and stale ticks do not re-arm the wait.
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
	if m.browseRate.rate != 0 {
		t.Errorf("first progress tick should seed the rate window, got rate %.2f", m.browseRate.rate)
	}
	if cmd == nil {
		t.Error("a live progress tick should re-arm the wait command")
	}

	m.browseRate.baseAt = time.Now().Add(-10 * time.Second)
	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 722000})
	if got := browseIndexRateLabel(m.browseRate.rate); got != "72k/s" {
		t.Errorf("recent progress rate = %q, want 72k/s", got)
	}

	m, _ = m.applyBrowseIndexProgress(browseIndexProgressMsg{gen: gen, n: 1000})
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
	m.browseRate.rate = 72000

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

// A closed progress channel ends the wait loop with a nil message.
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

// Index errors return to detail with redacted status and cleared browse state.
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

// Long-running indexing keeps a visible cancellation affordance.
func TestBrowseIndexingShowsCancelAffordance(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b"))
	m = next.(Model)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	if !m.isBrowseLoading || m.browseIndexed {
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

// Reopening an indexed snapshot reuses the session database without restic.
func TestBrowseReopenSkipsRestic(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/secret-dir", "secret-dir", true, 0)))
	m = openBrowse(t, m)
	if !strings.Contains(m.View().Content, "secret-dir") {
		t.Fatal("precondition: first browse should list the indexed entry")
	}

	m = update(t, m, press("q"))
	if m.view != detailView || m.browseRows != nil {
		t.Fatalf("leaving browse should return to detail and clear rows: view=%d rows=%v", m.view, m.browseRows)
	}

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

// Quitting cancels an in-flight index so restic cannot outlive the UI.
func TestBrowseQuitCancelsInFlightIndex(t *testing.T) {
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(time.Minute)}
	started := make(chan struct{})
	a.Restic = blockingRestic{started: started}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func(context.Context) (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	m := newTestModel(t, a)

	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected a browse command")
	}
	// Forward only the index result, not the progress waiter's nil message.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
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

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("index did not unblock after quit cancelled the context")
	}
}

// lineContaining returns the first matching line or fails the test.
func lineContaining(t *testing.T, s, sub string) string {
	t.Helper()
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", sub, s)
	return ""
}

// Responsive columns drop Owner, Perms, then Modified while retaining Name and Size.
func TestBrowseRendersMetadataColumnsResponsively(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)))

	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	wide := stripANSI(m.View().Content)
	for _, want := range []string{"Modified", "Perms", "Owner", "2026-05-26 11:28", "-rw-r--r--", "1000:1000"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide browse view missing %q\n---\n%s", want, wide)
		}
	}

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
		80:                      80,
		browseTableMaxWidth:     browseTableMaxWidth,
		browseTableMaxWidth + 1: browseTableMaxWidth,
		240:                     browseTableMaxWidth,
	}
	for in, want := range cases {
		if got := browseTableWidth(in); got != want {
			t.Errorf("browseTableWidth(%d) = %d, want %d", in, got, want)
		}
	}
}

// Wide terminals cap both header and data rows at browseTableMaxWidth.
func TestBrowseTableBoundedOnWideTerminal(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	m := openBrowse(t, newTestModel(t, browseApp(t,
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 240, Height: 40})
	view := stripANSI(m.View().Content)

	// Ignore View's terminal-width padding when measuring table content.
	row := strings.TrimRight(lineContaining(t, view, "file.txt"), " ")
	if w := lipgloss.Width(row); w > browseTableMaxWidth {
		t.Errorf("file row width = %d, want <= %d (table must be capped on a wide terminal)\n%q", w, browseTableMaxWidth, row)
	}
	hdr := strings.TrimRight(lineContaining(t, view, "Owner"), " ")
	if w := lipgloss.Width(hdr); w > browseTableMaxWidth {
		t.Errorf("header row width = %d, want <= %d\n%q", w, browseTableMaxWidth, hdr)
	}
}

// A wide U+FE55 rune must not shift columns or clip the owner value; display
// width, rather than rune count, determines the Name cell width.
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

// Explicit root ownership renders as 0:0; absent ownership renders as an em dash.
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

// Directory rows render recursive sizes, including "0 B" for empty directories.
func TestBrowseRendersDirectorySize(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/big", "big", true, 4096),
		bnode("/empty", "empty", true, 0),
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

// Help documents all open aliases while the compact footer omits right arrow.
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

// Full help documents sorting in the Browse section.
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

// The browse footer omits the removed load-more affordance.
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

func TestDetailFooterBrowseWording(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "browse") {
		t.Errorf("detail footer should advertise browse\n---\n%s", footer)
	}
	if strings.Contains(footer, "browse files") {
		t.Errorf("detail footer should read 'b browse', not 'browse files'\n---\n%s", footer)
	}
}

// extractTestSnapID has the 64-hex shape required by extraction planning.
const extractTestSnapID = "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"

// extractBrowseModel isolates extraction dispatch with valid browse state.
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

// Directory extraction derives its request from the selected browse entry.
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

// Regular-file extraction sets the file-mode attestation.
func TestBrowseExtractFileOpensSubModel(t *testing.T) {
	m := extractBrowseModel(t, []model.BrowseEntry{
		{Path: "/etc/hosts", Name: "hosts", Type: "file", Size: 412},
	}, 0)

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e on a file should open extractView, view = %d", m.view)
	}
	req := m.extract.req
	if req.Mode != app.ExtractFile {
		t.Errorf("Mode = %v, want ExtractFile", req.Mode)
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

// Unsupported node types are rejected without changing views or exposing paths.
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
			if strings.Contains(m.browseNotice, "thing") {
				t.Errorf("rejection notice leaked the entry name/path: %q", m.browseNotice)
			}
		})
	}
}

// Quitting cascades cancellation from the program context into extraction.
func TestBrowseExtractQuitCancelsInFlight(t *testing.T) {
	m := extractBrowseModel(t, []model.BrowseEntry{
		{Path: "/etc/hosts", Name: "hosts", Type: "file", Size: 1},
	}, 0)
	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("precondition: e should open extractView, view = %d", m.view)
	}

	// Block the driver until cancellation reaches it.
	drv := &fakeExtractDriver{}
	block := make(chan struct{})
	defer close(block)
	drv.push(extractResp{blockOn: block, err: context.Canceled})
	m.extract.drv = drv

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("precondition: enter should start the run, state = %v", m.extract.state)
	}

	// Observe only the worker result, not the progress pump's nil completion.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		go func() {
			if msg, ok := c().(extractRunDoneMsg); ok {
				done <- msg
			}
		}()
	}

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("extract did not unblock after quit cancelled the context")
	}
}

// The wide browse footer advertises extraction.
func TestBrowseFooterAdvertisesExtract(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/dir", "dir", true, 0))))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "e extract") {
		t.Errorf("browse footer should advertise 'e extract'\n---\n%s", footer)
	}
}

func TestExtractKeyBoundToE(t *testing.T) {
	keys := defaultKeys().Extract.Keys()
	if len(keys) != 1 || keys[0] != "e" {
		t.Errorf("Extract binding keys = %v, want [e]", keys)
	}
}

// sortBrowseApp uses attributes that give each sort mode a distinct order.
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

// Sorting cycles name, size, and modified order without moving selection.
func TestBrowseSortCyclesAndPreservesCursor(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))

	nameOrder := []string{"/dir", "/a.txt", "/b.txt", "/c.txt"}
	sizeOrder := []string{"/dir", "/b.txt", "/c.txt", "/a.txt"}
	modOrder := []string{"/dir", "/c.txt", "/a.txt", "/b.txt"}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, nameOrder) {
		t.Fatalf("initial listing = %v, want canonical %v", got, nameOrder)
	}

	m = update(t, m, press("j"))
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

// Loading freezes sort changes while rows are pending replacement.
func TestBrowseSortIgnoredWhileLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, press("o"))
	if m.browseSortMode != browseSortSize {
		t.Fatalf("precondition: first o should select size, got %q", m.browseSortMode.label())
	}
	frozen := browseRowPaths(m.browseRows)

	m.isBrowseLoading = true
	m = update(t, m, press("o"))
	if m.browseSortMode != browseSortSize {
		t.Errorf("o while loading should not advance the sort, got %q", m.browseSortMode.label())
	}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, frozen) {
		t.Errorf("o while loading should not reorder rows: got %v want %v", got, frozen)
	}
}

// Subdirectories inherit the active browse sort.
func TestBrowseSortInheritedIntoSubdir(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/dir", "dir", true, 0),
		bnode("/dir/a.txt", "a.txt", false, 100),
		bnode("/dir/z.bin", "z.bin", false, 300),
	)))
	m = update(t, m, press("o"))
	if m.browseSortMode != browseSortSize {
		t.Fatalf("precondition: o should select size, got %q", m.browseSortMode.label())
	}

	m = pressBrowse(t, m, "enter")
	if m.browseDir != "/dir" {
		t.Fatalf("should have descended into /dir, browseDir = %q", m.browseDir)
	}
	if m.browseSortMode != browseSortSize {
		t.Errorf("descending should keep the active sort, got %q", m.browseSortMode.label())
	}
	want := []string{"/dir/z.bin", "/dir/a.txt"}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, want) {
		t.Errorf("subdir listing = %v, want size order %v", got, want)
	}
}

// Sorting preserves canonical cached rows and never requeries visited directories.
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

	m = update(t, m, press("o"))
	if m.browseSortMode != browseSortName {
		t.Fatalf("three cycles should return to name, got %q", m.browseSortMode.label())
	}
	if got := browseRowPaths(m.browseRows); !reflect.DeepEqual(got, canonical) {
		t.Errorf("name mode should restore canonical order: %v want %v", got, canonical)
	}

	m = pressBrowse(t, m, "enter")
	m = pressBrowse(t, m, "backspace")
	if m.browseDir != "/" {
		t.Fatalf("should be back at /, browseDir = %q", m.browseDir)
	}
	if store.listCount("/") != 1 {
		t.Errorf("/ was re-queried after sorting+navigation: ListDir(/) = %d, want 1", store.listCount("/"))
	}
}

// Sorting a suspended search changes only the visible directory listing.
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

	m = update(t, m, press("o"))
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

// The summary labels non-default sorts only.
func TestBrowseSortSummaryIndicator(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))

	if line := m.browseSummaryLine(); strings.Contains(line, "sort:") {
		t.Errorf("name mode should omit the sort indicator, got %q", line)
	}

	m = update(t, m, press("o"))
	if line := m.browseSummaryLine(); !strings.Contains(line, "sort: size") {
		t.Errorf("summary should show the size sort, got %q", line)
	}

	m = update(t, m, press("o"))
	if line := m.browseSummaryLine(); !strings.Contains(line, "sort: modified") {
		t.Errorf("summary should show the modified sort, got %q", line)
	}

	m = update(t, m, press("o"))
	if line := m.browseSummaryLine(); strings.Contains(line, "sort:") {
		t.Errorf("cycling back to name should drop the sort indicator, got %q", line)
	}
}

func TestBrowseFooterAdvertisesSort(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "sort") {
		t.Errorf("browse footer should advertise the sort key\n---\n%s", footer)
	}
}

// The directory header marks exactly one active sort column without shifting its label.
func TestBrowseSortHeaderArrow(t *testing.T) {
	m := openBrowse(t, newTestModel(t, sortBrowseApp(t)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})

	header := func() string { return lineContaining(t, stripANSI(m.View().Content), "Owner") }

	hdr := header()
	if !strings.Contains(hdr, "Name ↓") {
		t.Errorf("name sort should mark Name with ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "↓ Size") || strings.Contains(hdr, "Modified ↓") {
		t.Errorf("name sort should not mark Size/Modified\n%q", hdr)
	}

	// Size's arrow leads its right-aligned label.
	m = update(t, m, press("o"))
	hdr = header()
	if !strings.Contains(hdr, "↓ Size") {
		t.Errorf("size sort should mark Size with a leading ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "Name ↓") || strings.Contains(hdr, "Modified ↓") {
		t.Errorf("size sort should mark only Size\n%q", hdr)
	}

	m = update(t, m, press("o"))
	hdr = header()
	if !strings.Contains(hdr, "Modified ↓") {
		t.Errorf("modified sort should mark Modified with ↓\n%q", hdr)
	}
	if strings.Contains(hdr, "Name ↓") || strings.Contains(hdr, "↓ Size") {
		t.Errorf("modified sort should mark only Modified\n%q", hdr)
	}
}

// Relevance-ranked search results never display a column-sort arrow.
func TestBrowseSearchHeaderHasNoSortArrow(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 40})
	m = update(t, m, press("o"))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	hdr := lineContaining(t, stripANSI(m.View().Content), "Owner")
	if strings.ContainsAny(hdr, "↑↓") {
		t.Errorf("search header must carry no sort arrow\n%q", hdr)
	}
}
