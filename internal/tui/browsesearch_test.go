package tui

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// These state-transition tests share the production scorer and protect filename
// clearing and path-free error behavior.

// openSearch opens the global filename search from an indexed, idle browse view.
func openSearch(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("/"))
	if !m.browseSearching {
		t.Fatal("/ should open the global filename search in an indexed browse view")
	}
	return m
}

// typeSearch enters q and delivers each asynchronous search result. Synchronous
// query clearing emits no command and is skipped.
func typeSearch(t *testing.T, m Model, q string) Model {
	t.Helper()
	for _, r := range q {
		next, cmd := m.Update(press(string(r)))
		m = next.(Model)
		if cmd == nil {
			continue
		}
		msg, ok := cmd().(browseSearchMsg)
		if !ok {
			t.Fatalf("typing %q produced %T, want browseSearchMsg", string(r), cmd())
		}
		m = update(t, m, msg)
	}
	return m
}

// Search activation requires an indexed, idle browse session.
func TestBrowseSearchActivationRequiresIndexedIdle(t *testing.T) {
	m := newTestModel(t, browseApp(t, bnode("/a", "a", true, 0)))
	m = update(t, m, press("enter"))
	next, _ := m.Update(press("b")) // start indexing; do not run the index command
	m = next.(Model)
	if !m.isBrowseLoading || m.browseIndexed {
		t.Fatal("precondition: should be mid-index (loading, not indexed)")
	}
	if mid := update(t, m, press("/")); mid.browseSearching {
		t.Error("/ must not open search while indexing")
	}

	indexed := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a", "a", true, 0))))
	indexed = update(t, indexed, press("/"))
	if !indexed.browseSearching {
		t.Error("/ should open search once the snapshot is indexed and idle")
	}
	if indexed.browseSearchQuery != "" || indexed.browseSearchRows != nil || indexed.browseSearchCursor != 0 {
		t.Errorf("opening search should start from a clean slate: q=%q rows=%v cur=%d",
			indexed.browseSearchQuery, indexed.browseSearchRows, indexed.browseSearchCursor)
	}
}

// Live search returns ranked matches and an uncapped snapshot-wide total.
func TestBrowseSearchTypePopulates(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
		bnode("/home/photo.jpg", "photo.jpg", false, 20),
		bnode("/etc", "etc", true, 0),
		bnode("/etc/report.conf", "report.conf", false, 5),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	if m.browseSearchTotal != 2 {
		t.Errorf("'report' should match 2 files across dirs, total = %d", m.browseSearchTotal)
	}
	got := map[string]bool{}
	for _, e := range m.browseSearchRows {
		got[e.Path] = true
	}
	for _, want := range []string{"/home/report.txt", "/etc/report.conf"} {
		if !got[want] {
			t.Errorf("search result missing %q, got %+v", want, m.browseSearchRows)
		}
	}
	if got["/home/photo.jpg"] {
		t.Errorf("non-matching file leaked into results: %+v", m.browseSearchRows)
	}
}

// Results from stale generations or edited queries cannot replace visible state.
func TestBrowseSearchStaleResultsDropped(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/alpha.txt", "alpha.txt", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "alpha")
	if len(m.browseSearchRows) != 1 {
		t.Fatalf("precondition: 'alpha' should match once, got %d", len(m.browseSearchRows))
	}
	gen, query := m.browseGen, m.browseSearchQuery
	leak := model.BrowseSearchResult{Rows: []model.BrowseEntry{{Path: "/zzz/secret.txt", Name: "secret.txt"}}, Total: 1}

	m = update(t, m, browseSearchMsg{gen: gen + 99, query: query, result: leak})    // stale generation
	m = update(t, m, browseSearchMsg{gen: gen, query: query + "zzz", result: leak}) // edited-past query

	if len(m.browseSearchRows) != 1 || m.browseSearchRows[0].Path != "/home/alpha.txt" {
		t.Errorf("a stale/mismatched result must not replace the rows: %+v", m.browseSearchRows)
	}
	if strings.Contains(m.View().Content, "secret") {
		t.Errorf("a dropped result leaked into the view\n---\n%s", m.View().Content)
	}
}

// Escape cancels search without disturbing directory-list state.
func TestBrowseSearchEscRestoresListing(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/b/inner.txt", "inner.txt", false, 1),
	)))
	m = pressBrowse(t, m, "j")
	m = pressBrowse(t, m, "enter")
	dirBefore, curBefore := m.browseDir, m.browseCursor
	rowsBefore := m.browseRows
	if dirBefore != "/b" {
		t.Fatalf("precondition: browseDir = %q, want /b", dirBefore)
	}

	m = openSearch(t, m)
	m = typeSearch(t, m, "inner")
	m = update(t, m, press("esc"))

	if m.browseSearching {
		t.Error("esc should close the search")
	}
	if m.view != browseView {
		t.Errorf("esc should stay in browse, view = %d", m.view)
	}
	if m.browseDir != dirBefore || m.browseCursor != curBefore {
		t.Errorf("esc must restore the listing exactly: dir=%q cursor=%d, want %q/%d",
			m.browseDir, m.browseCursor, dirBefore, curBefore)
	}
	if len(m.browseRows) != len(rowsBefore) || (len(rowsBefore) > 0 && m.browseRows[0].Path != rowsBefore[0].Path) {
		t.Errorf("esc must leave the directory rows untouched: %+v", m.browseRows)
	}
	if m.browseSearchQuery != "" || m.browseSearchRows != nil {
		t.Errorf("esc must clear search state: q=%q rows=%v", m.browseSearchQuery, m.browseSearchRows)
	}
}

// Enter opens a match's parent with the file selected and search suspended.
func TestBrowseSearchEnterNavigatesToMatch(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/sub", "sub", true, 0),
		bnode("/home/sub/target.txt", "target.txt", false, 5),
		bnode("/home/other.txt", "other.txt", false, 3),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "target")
	if len(m.browseSearchRows) != 1 || m.browseSearchRows[0].Path != "/home/sub/target.txt" {
		t.Fatalf("precondition: search should find the target, got %+v", m.browseSearchRows)
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

	if m.browseSearching {
		t.Error("enter should close the search input")
	}
	if !m.browseSearchSuspended {
		t.Error("enter should suspend (park) the search so esc can restore the results")
	}
	if len(m.browseSearchRows) == 0 {
		t.Error("a suspended search must keep its result rows for restore")
	}
	if m.browseDir != "/home/sub" {
		t.Errorf("enter should open the match's parent dir, browseDir = %q want /home/sub", m.browseDir)
	}
	if sel := m.selectedBrowseEntry(); sel == nil || sel.Path != "/home/sub/target.txt" {
		t.Errorf("the cursor should land on the match, got %+v", sel)
	}
}

// Enter without a match closes search without navigating.
func TestBrowseSearchEnterNoMatchCloses(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a.txt", "a.txt", false, 1),
	)))
	dirBefore := m.browseDir
	m = openSearch(t, m)
	m = typeSearch(t, m, "zzznomatch")
	if len(m.browseSearchRows) != 0 {
		t.Fatalf("precondition: query should have no matches, got %+v", m.browseSearchRows)
	}

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd != nil {
		t.Error("enter with no match should emit no command")
	}
	if m.browseSearching {
		t.Error("enter with no match should close the search")
	}
	if m.browseDir != dirBefore {
		t.Errorf("enter with no match should leave the listing unchanged, dir = %q", m.browseDir)
	}
}

// Suspended search restores its query, rows, and cursor for another selection.
func TestBrowseSearchSuspendedEscRestores(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/proj", "proj", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
		bnode("/proj/report.md", "report.md", false, 8),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")
	if len(m.browseSearchRows) != 2 {
		t.Fatalf("precondition: 'report' should match 2 files across dirs, got %d", len(m.browseSearchRows))
	}

	// Use the second result to prove cursor restoration without assuming rank.
	m = update(t, m, press("down"))
	wantCursor := m.browseSearchCursor
	if wantCursor != 1 {
		t.Fatalf("precondition: cursor should be on the second match, got %d", wantCursor)
	}
	wantQuery := m.browseSearchQuery
	sel := m.selectedBrowseSearchEntry()
	if sel == nil {
		t.Fatal("precondition: a match should be selected")
	}
	wantPath, wantDir := sel.Path, path.Dir(sel.Path)

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
	if m.browseSearching || !m.browseSearchSuspended {
		t.Fatalf("enter should suspend the search: searching=%v suspended=%v", m.browseSearching, m.browseSearchSuspended)
	}
	if m.browseDir != wantDir {
		t.Errorf("enter should open the match's parent dir, browseDir = %q want %q", m.browseDir, wantDir)
	}
	if cur := m.selectedBrowseEntry(); cur == nil || cur.Path != wantPath {
		t.Errorf("the cursor should land on the match, got %+v want %q", cur, wantPath)
	}

	m = update(t, m, press("esc"))
	if !m.browseSearching || m.browseSearchSuspended {
		t.Fatalf("esc should restore the search overlay: searching=%v suspended=%v", m.browseSearching, m.browseSearchSuspended)
	}
	if m.browseSearchQuery != wantQuery {
		t.Errorf("restored query = %q, want %q", m.browseSearchQuery, wantQuery)
	}
	if len(m.browseSearchRows) != 2 {
		t.Errorf("restored search should keep its 2 rows, got %d", len(m.browseSearchRows))
	}
	if m.browseSearchCursor != wantCursor {
		t.Errorf("restored cursor = %d, want %d", m.browseSearchCursor, wantCursor)
	}

	// Cached directories may make the second jump synchronous.
	next, cmd = m.Update(press("enter"))
	m = next.(Model)
	if cmd != nil {
		if msg, ok := cmd().(browseDirMsg); ok {
			m = update(t, m, msg)
		}
	}
	if m.browseSearching || !m.browseSearchSuspended {
		t.Errorf("a second enter should suspend the search again: searching=%v suspended=%v", m.browseSearching, m.browseSearchSuspended)
	}
}

// q leaves a parked search and clears every filename-bearing search field.
func TestBrowseSearchSuspendedQuitLeavesBrowse(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd != nil {
		if msg, ok := cmd().(browseDirMsg); ok {
			m = update(t, m, msg)
		}
	}
	if !m.browseSearchSuspended {
		t.Fatal("precondition: search should be suspended after enter on a match")
	}

	m = update(t, m, press("q"))
	if m.view != detailView {
		t.Errorf("q should leave browse to the detail view, view = %d", m.view)
	}
	if m.browseSearchSuspended || m.browseSearchRows != nil || m.browseSearchQuery != "" {
		t.Errorf("leaving browse must wipe parked search state: suspended=%v rows=%v q=%q",
			m.browseSearchSuspended, m.browseSearchRows, m.browseSearchQuery)
	}
}

// A parked-search footer distinguishes restoring results from leaving browse.
func TestBrowseSearchSuspendedHeaderShowsResults(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd != nil {
		if msg, ok := cmd().(browseDirMsg); ok {
			m = update(t, m, msg)
		}
	}
	if !m.browseSearchSuspended {
		t.Fatal("precondition: search should be suspended")
	}

	h := stripANSI(m.footerView())
	if !strings.Contains(h, "esc results") {
		t.Errorf("a parked search footer should advertise esc → results\n---\n%s", h)
	}
	if !strings.Contains(h, "q back") {
		t.Errorf("a parked search footer should still advertise q → back\n---\n%s", h)
	}
	if strings.Contains(h, "esc cancel") {
		t.Errorf("a parked search footer is not the open-input state\n---\n%s", h)
	}
}

// Restoring and cancelling search before a jump listing returns must clear
// isBrowseLoading; otherwise dropping the stale result would freeze navigation.
func TestBrowseSearchRestoreWhileLoadingClearsLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/etc", "etc", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	// Hold the jump result to keep the directory listing in flight.
	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter on a match in an unlisted dir should start an async listing")
	}
	if !m.isBrowseLoading {
		t.Fatal("precondition: the jumped-to listing should be in flight (isBrowseLoading)")
	}
	stale := cmd().(browseDirMsg)

	m = update(t, m, press("esc"))
	if !m.browseSearching {
		t.Fatal("first esc should restore the search overlay")
	}
	if m.isBrowseLoading {
		t.Error("restoring search must drop the in-flight jump listing and clear isBrowseLoading")
	}

	// Return the stale listing after cancelling the restored search.
	m = update(t, m, press("esc"))
	if m.browseSearching {
		t.Fatal("second esc should cancel the restored search")
	}
	m = update(t, m, stale)
	if m.isBrowseLoading {
		t.Error("isBrowseLoading must stay cleared after the stale jump listing is dropped, not stuck true")
	}

	// Cursor movement proves navigation is no longer frozen.
	m = update(t, m, press("j"))
	if m.browseCursor != 1 {
		t.Errorf("browse navigation should work after restore+cancel, cursor = %d want 1", m.browseCursor)
	}
}

// Refiring search after restoring an in-flight jump must also clear browse loading.
func TestBrowseSearchRestoreThenTypeClearsLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/etc", "etc", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
		bnode("/home/reportx.txt", "reportx.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd == nil || !m.isBrowseLoading {
		t.Fatal("precondition: enter should start an in-flight jump listing")
	}
	stale := cmd().(browseDirMsg)

	m = update(t, m, press("esc"))
	if !m.browseSearching || m.isBrowseLoading {
		t.Fatalf("restore should reopen search and clear loading: searching=%v loading=%v", m.browseSearching, m.isBrowseLoading)
	}

	m = typeSearch(t, m, "x")
	if m.isBrowseLoading {
		t.Error("a refired search must not set isBrowseLoading")
	}

	m = update(t, m, stale)

	m = update(t, m, press("esc"))
	if m.browseSearching {
		t.Fatal("esc should cancel the restored search")
	}
	if m.isBrowseLoading {
		t.Error("isBrowseLoading must be clear after restore→type→cancel, not stuck true")
	}
	m = update(t, m, press("j"))
	if m.browseCursor != 1 {
		t.Errorf("browse navigation should work afterward, cursor = %d want 1", m.browseCursor)
	}
}

// Enter cannot accept rows from the previous query while a new scan is pending.
func TestBrowseSearchEnterDropsStaleRowAfterEdit(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")
	if len(m.browseSearchRows) != 1 || m.browseSearchShownQuery != "report" {
		t.Fatalf("precondition: 'report' should be the shown query with one row, got shown=%q rows=%+v",
			m.browseSearchShownQuery, m.browseSearchRows)
	}

	// Hold the new result so visible rows still belong to the previous query.
	next, cmd := m.Update(press("z"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("editing the query should dispatch a new search")
	}
	if m.browseSearchQuery != "reportz" || m.browseSearchShownQuery != "report" {
		t.Fatalf("after edit: query=%q shown=%q, want reportz/report", m.browseSearchQuery, m.browseSearchShownQuery)
	}

	next, navCmd := m.Update(press("enter"))
	m = next.(Model)
	if navCmd != nil {
		t.Error("Enter on a query whose result is still in flight must not navigate")
	}
	if !m.browseSearching {
		t.Error("Enter on stale rows must keep the search open, not accept and close it")
	}
	if m.browseDir != "/" {
		t.Errorf("Enter must not change the listing, browseDir = %q want /", m.browseDir)
	}

	m = update(t, m, cmd().(browseSearchMsg))
	if m.browseSearchShownQuery != "reportz" {
		t.Errorf("after the scan returns, shown query = %q, want reportz", m.browseSearchShownQuery)
	}
}

func TestBrowseSearchBackspaceTrimsAndRefires(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 1),
		bnode("/home/rep.md", "rep.md", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "repo")
	if len(m.browseSearchRows) != 1 || m.browseSearchRows[0].Path != "/home/report.txt" {
		t.Fatalf("precondition: 'repo' should match only report.txt, got %+v", m.browseSearchRows)
	}

	next, cmd := m.Update(press("backspace"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("backspace should refire the search")
	}
	m = update(t, m, cmd().(browseSearchMsg))

	if m.browseSearchQuery != "rep" {
		t.Errorf("query = %q, want rep after one backspace", m.browseSearchQuery)
	}
	if len(m.browseSearchRows) != 2 {
		t.Errorf("'rep' should broaden to both files, got %d: %+v", len(m.browseSearchRows), m.browseSearchRows)
	}
}

// Empty queries clear synchronously without closing search or scanning storage.
func TestBrowseSearchEmptyQueryClearsSynchronously(t *testing.T) {
	a, store := browseAppWithStore(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a.txt", "a.txt", false, 1),
	)
	m := openBrowse(t, newTestModel(t, a))
	m = openSearch(t, m)
	m = typeSearch(t, m, "a")
	if len(m.browseSearchRows) == 0 {
		t.Fatal("precondition: 'a' should match")
	}
	scansBefore := store.searchCalls()

	next, cmd := m.Update(press("backspace"))
	m = next.(Model)
	if cmd != nil {
		t.Error("emptying the query must clear synchronously, with no command")
	}
	if got := store.searchCalls(); got != scansBefore {
		t.Errorf("emptying the query must not scan the store: %d scans, want %d", got, scansBefore)
	}
	if m.browseSearchQuery != "" {
		t.Errorf("query = %q, want empty", m.browseSearchQuery)
	}
	if m.browseSearchRows != nil || m.browseSearchTotal != 0 {
		t.Errorf("emptying the query must clear results: rows=%v total=%d", m.browseSearchRows, m.browseSearchTotal)
	}
	if !m.browseSearching {
		t.Error("emptying the query keeps the search open (just empties it)")
	}
}

// Whitespace-only queries accept like empty queries rather than stale results.
func TestBrowseSearchWhitespaceQueryEnterCloses(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a.txt", "a.txt", false, 1),
	)))
	dirBefore := m.browseDir
	m = openSearch(t, m)

	next, cmd := m.Update(press(" "))
	m = next.(Model)
	if cmd != nil {
		t.Error("a whitespace-only query must clear synchronously, with no command")
	}
	if m.browseSearchQuery != " " {
		t.Fatalf("query = %q, want a single space", m.browseSearchQuery)
	}

	next, navCmd := m.Update(press("enter"))
	m = next.(Model)
	if navCmd != nil {
		t.Error("Enter on a whitespace-only query should emit no navigation command")
	}
	if m.browseSearching {
		t.Error("Enter on a whitespace-only query should close the search, like an empty one")
	}
	if m.browseDir != dirBefore {
		t.Errorf("Enter on a whitespace-only query must leave the listing unchanged, dir = %q", m.browseDir)
	}
}

// Search help uses open/cancel rather than filter-specific apply/clear wording.
func TestBrowseSearchFooterHelpWording(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/home", "home", true, 0))))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	m = openSearch(t, m)

	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "open") {
		t.Errorf("search footer should advertise enter → open\n---\n%s", footer)
	}
	if !strings.Contains(footer, "cancel") {
		t.Errorf("search footer should advertise esc → cancel\n---\n%s", footer)
	}
	if strings.Contains(footer, "apply") || strings.Contains(footer, "clear") {
		t.Errorf("search footer must not reuse the filter's apply/clear wording\n---\n%s", footer)
	}
}

func TestBrowseSearchHardQuitQuits(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a.txt", "a.txt", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "a")

	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = next.(Model)
	if !m.quitting {
		t.Error("ctrl+c while searching should quit")
	}
	if cmd == nil {
		t.Error("ctrl+c while searching should emit a quit command")
	}
}

// Printable browse bindings remain literal text while search input is open.
func TestBrowseSearchPrintableKeysAreLiteral(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/qsh", "qsh", true, 0),
	)))
	m = openSearch(t, m)
	for _, k := range []string{"q", "s", "?", "h", "l", "j", "k"} {
		next, cmd := m.Update(press(k))
		m = next.(Model)
		if !m.browseSearching {
			t.Fatalf("%q must not leave search mode", k)
		}
		if m.view != browseView {
			t.Fatalf("%q must not change the view while searching (view = %d)", k, m.view)
		}
		if cmd != nil {
			m = update(t, m, cmd().(browseSearchMsg))
		}
	}
	if m.browseSearchQuery != "qs?hljk" {
		t.Errorf("printable keys should accumulate as literal text: query = %q, want qs?hljk", m.browseSearchQuery)
	}
}

// Arrows and ctrl+j/ctrl+k navigate results while plain j/k edit the query.
func TestBrowseSearchResultNavigationKeys(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/job1.txt", "job1.txt", false, 1),
		bnode("/home/job2.txt", "job2.txt", false, 1),
		bnode("/home/job3.txt", "job3.txt", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "job")
	if len(m.browseSearchRows) != 3 {
		t.Fatalf("precondition: 'job' should match 3 files, got %d", len(m.browseSearchRows))
	}

	ctrl := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }
	steps := []struct {
		name    string
		msg     tea.KeyPressMsg
		wantCur int
	}{
		{"down", press("down"), 1},
		{"ctrl+j", ctrl('j'), 2},
		{"up", press("up"), 1},
		{"ctrl+k", ctrl('k'), 0},
	}
	for _, s := range steps {
		next, cmd := m.Update(s.msg)
		m = next.(Model)
		if cmd != nil {
			t.Errorf("%s should be a cursor move, not a search dispatch", s.name)
		}
		if m.browseSearchCursor != s.wantCur {
			t.Errorf("after %s cursor = %d, want %d", s.name, m.browseSearchCursor, s.wantCur)
		}
		if m.browseSearchQuery != "job" {
			t.Errorf("%s must not edit the query, got %q", s.name, m.browseSearchQuery)
		}
	}

	next, cmd := m.Update(press("k"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("plain k should be typed into the query and refire the search")
	}
	if m.browseSearchQuery != "jobk" {
		t.Errorf("plain k should append to the query, got %q want jobk", m.browseSearchQuery)
	}
}

// clearBrowse removes filenames from every active or suspended search field.
func TestBrowseSearchClearBrowseWipesSearchState(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/secret.txt", "secret.txt", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "secret")
	if len(m.browseSearchRows) == 0 || !m.browseSearching {
		t.Fatal("precondition: search should be open with matches")
	}
	// Exercise the suspended state where data may outlive the overlay but not browse.
	m.browseSearchSuspended = true

	c := m.clearBrowse()
	if c.browseSearching {
		t.Error("clearBrowse must close search")
	}
	if c.browseSearchSuspended || c.browseSearchQuery != "" || c.browseSearchShownQuery != "" || c.browseSearchRows != nil ||
		c.browseSearchCursor != 0 || c.browseSearchTotal != 0 || c.browseSearchErr != "" {
		t.Errorf("clearBrowse must zero every browseSearch* field (privacy): suspended=%v q=%q shown=%q rows=%v cur=%d total=%d err=%q",
			c.browseSearchSuspended, c.browseSearchQuery, c.browseSearchShownQuery, c.browseSearchRows, c.browseSearchCursor, c.browseSearchTotal, c.browseSearchErr)
	}
}

func TestBrowseSearchSummaryStates(t *testing.T) {
	m := newTestModel(t, browseApp(t))
	m.browseSearching = true

	if got := m.browseSearchSummary(); got != "type to search" {
		t.Errorf("empty query summary = %q, want 'type to search'", got)
	}
	m.browseSearchQuery = "zzz"
	if got := m.browseSearchSummary(); got != "(no matches)" {
		t.Errorf("no-match summary = %q, want '(no matches)'", got)
	}
	m.browseSearchRows = []model.BrowseEntry{{Path: "/a", Name: "a"}, {Path: "/b", Name: "b"}}
	m.browseSearchTotal = 2
	if got := m.browseSearchSummary(); got != "2 matches" {
		t.Errorf("all-shown summary = %q, want '2 matches'", got)
	}
	m.browseSearchTotal = 250
	if got := m.browseSearchSummary(); got != "showing 2 of 250 matches" {
		t.Errorf("capped summary = %q, want 'showing 2 of 250 matches'", got)
	}
	m.browseSearchErr = "browse search: store unavailable"
	if got := m.browseSearchSummary(); got != "browse search: store unavailable" {
		t.Errorf("error summary = %q, want the error to take precedence", got)
	}
}

// Capped results retain the true total in the footer.
func TestBrowseSearchFooterShowsCappedNote(t *testing.T) {
	nodes := []model.BrowseNode{bnode("/home", "home", true, 0)}
	for i := range browseSearchResultLimit + 5 {
		name := fmt.Sprintf("match-%04d.log", i)
		nodes = append(nodes, bnode("/home/"+name, name, false, 1))
	}
	m := openBrowse(t, newTestModel(t, browseApp(t, nodes...)))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	m = typeSearch(t, m, "match")

	if m.browseSearchTotal != browseSearchResultLimit+5 {
		t.Fatalf("Total = %d, want %d", m.browseSearchTotal, browseSearchResultLimit+5)
	}
	if len(m.browseSearchRows) != browseSearchResultLimit {
		t.Fatalf("rows should be capped to %d, got %d", browseSearchResultLimit, len(m.browseSearchRows))
	}
	footer := stripANSI(m.footerView())
	want := fmt.Sprintf("showing %d of %d", browseSearchResultLimit, browseSearchResultLimit+5)
	if !strings.Contains(footer, want) {
		t.Errorf("footer should show the capped note %q\n---\n%s", want, footer)
	}
}

// Store errors remain visible without exposing a filename.
func TestBrowseSearchErrorVisibleAndPathFree(t *testing.T) {
	a, store := browseAppWithStore(t,
		bnode("/home", "home", true, 0),
		bnode("/home/topsecret.txt", "topsecret.txt", false, 1),
	)
	m := openBrowse(t, newTestModel(t, a))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	store.failSearches(errors.New("browse store unavailable"))
	// The query may be echoed, but the matching filename must remain absent.
	m = typeSearch(t, m, "secret")

	if m.browseSearchErr == "" {
		t.Error("a failed search should set a visible error")
	}
	if !m.browseSearching {
		t.Error("a failed search should keep search open so the error is visible")
	}
	if len(m.browseSearchRows) != 0 || m.browseSearchTotal != 0 {
		t.Errorf("a failed search should clear rows/total: rows=%v total=%d", m.browseSearchRows, m.browseSearchTotal)
	}
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "browse search: browse store unavailable") {
		t.Errorf("footer should surface the path-free search error\n---\n%s", footer)
	}
	if strings.Contains(footer, "topsecret") {
		t.Errorf("the search error/footer must not leak a filename\n---\n%s", footer)
	}
}

// Search rows show full paths and clip overlong paths without wrapping.
func TestBrowseSearchRowsRenderFullPaths(t *testing.T) {
	full := "/home/deep/needle.txt"
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode(full, "needle.txt", false, 1),
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	m = typeSearch(t, m, "needle")

	wide := stripANSI(m.View().Content)
	if !strings.Contains(wide, full) {
		t.Errorf("a search result should render its full path %q\n---\n%s", full, wide)
	}
	hdr := lineContaining(t, wide, "Size")
	if !strings.Contains(hdr, "Path") {
		t.Errorf("the search list flex column should be labelled 'Path', got %q", hdr)
	}

	// Clipping preserves the basename that identifies the match.
	m = update(t, m, tea.WindowSizeMsg{Width: 32, Height: 20})
	narrow := stripANSI(m.View().Content)
	tw := browseTableWidth(32)
	row := lineContaining(t, narrow, "needle.txt")
	if w := lipgloss.Width(strings.TrimRight(row, " ")); w > tw {
		t.Errorf("a clipped search row width = %d, want <= %d: %q", w, tw, row)
	}
	if !strings.Contains(row, "…/needle.txt") {
		t.Errorf("an overlong path should elide the middle but keep the basename: %q", row)
	}
}

// Search mode replaces the browse footer's q-back hint with esc-cancel.
func TestBrowseSearchHeaderShowsCancelLabel(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/home", "home", true, 0))))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	if h := stripANSI(m.footerView()); !strings.Contains(h, "q back") {
		t.Errorf("browse footer should advertise 'q back' when not searching\n---\n%s", h)
	}

	m = openSearch(t, m)
	h := stripANSI(m.footerView())
	if strings.Contains(h, "q back") {
		t.Errorf("while searching the footer must not say 'q back' (q is literal)\n---\n%s", h)
	}
	if !strings.Contains(h, "esc cancel") {
		t.Errorf("while searching the footer should say 'esc cancel'\n---\n%s", h)
	}
}

func TestBrowseHelpDocumentsSearch(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/home", "home", true, 0))))
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})

	if footer := stripANSI(m.footerView()); !strings.Contains(footer, "search") {
		t.Errorf("browse footer should advertise search\n---\n%s", footer)
	}

	_, right := m.helpColumns()
	var browse helpSection
	for _, s := range right {
		if s.title == "Browse" {
			browse = s
		}
	}
	if browse.title == "" {
		t.Fatal("help overlay missing the Browse section")
	}
	found := false
	for _, e := range browse.entries {
		if e.desc == "search filenames" {
			found = true
			if e.keys != "/" {
				t.Errorf("Browse search row keys = %q, want /", e.keys)
			}
		}
	}
	if !found {
		t.Error("Browse help section missing the 'search filenames' row")
	}
}
