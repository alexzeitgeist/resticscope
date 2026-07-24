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

// browsesearch_test.go drives the global fuzzy filename search added to the
// snapshot browser. The tests are state-transition tests (no pixels): they press
// keys, run the resulting commands through the in-memory fakeBrowseStore — which
// ranks with the real model scorer so DB and TUI ranking cannot drift — and assert
// the model's search state and the rendered footer/list. Two invariants are guarded
// directly: filenames never linger once search/browse is left, and a store error
// surfaces path-free.

// openSearch opens the global filename search from an indexed, idle browse view.
func openSearch(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("/"))
	if !m.browseSearching {
		t.Fatal("/ should open the global filename search in an indexed browse view")
	}
	return m
}

// typeSearch sends each rune of q to the open search input, runs the search
// command, and delivers its browseSearchMsg so the model lands on the result for
// the full query. A keystroke that emits no command (a whitespace-only query that
// clears synchronously) is skipped, mirroring the live behaviour.
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

// Activation is gated on an indexed, idle browse: there is nothing to search
// before the one-time crawl commits, and navigation (including opening search) is
// paused while a load is in flight.
func TestBrowseSearchActivationRequiresIndexedIdle(t *testing.T) {
	// Mid-index: "/" must not open search.
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

	// Indexed and idle: "/" opens the search input, reset to an empty query.
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

// Typing fires a live search and populates the ranked matches and the true total,
// drawn from anywhere in the snapshot (not just the current directory).
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

// A late search result whose generation or query no longer matches the model (a
// superseded keystroke, or one the user has since edited past) is dropped and
// never overwrites fresher state or leaks into the view.
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

// Esc cancels the search and restores the underlying directory listing exactly —
// the separate search state never disturbs browseDir/browseRows/browseCursor.
func TestBrowseSearchEscRestoresListing(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/b/inner.txt", "inner.txt", false, 1),
	)))
	m = pressBrowse(t, m, "j")     // cursor onto /b
	m = pressBrowse(t, m, "enter") // descend into /b
	dirBefore, curBefore := m.browseDir, m.browseCursor
	rowsBefore := m.browseRows
	if dirBefore != "/b" {
		t.Fatalf("precondition: browseDir = %q, want /b", dirBefore)
	}

	m = openSearch(t, m)
	m = typeSearch(t, m, "inner")
	m = update(t, m, press("esc")) // cancel

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

// Enter on a match opens that match's parent directory with the file preselected and
// suspends the search (the input closes but the result set is parked so esc can
// restore it) — so a global hit lands the user in the right folder.
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

// Enter with no selected match (an empty result) just closes the search, leaving
// the prior listing in place and emitting no navigation command.
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

// Enter suspends (parks) the search rather than exiting it: esc from the jumped-to
// listing restores the overlay with the same query, rows, and cursor, and a second
// Enter jumps again. This is the "open a result, go back to the result set, pick
// another" file-picker flow, kept within the no-filenames-linger-after-browse rule.
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

	// Move onto the second match so the restored cursor is provably preserved (not 0),
	// and capture its path without assuming a ranking order.
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

	// Enter suspends the search and jumps to the match's parent directory.
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

	// esc restores the parked search overlay with the same query, rows, and cursor.
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

	// A second Enter from the restored search jumps again (the shown/live accept gate
	// still matches). It may serve a cached dir synchronously, so accept either a
	// command or no command; only the suspend transition is asserted.
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

// q is the escape hatch out of a parked search: unlike esc (which restores the
// results), q leaves browse outright and clearBrowse wipes every search field, so no
// filename lingers once the user leaves browse.
func TestBrowseSearchSuspendedQuitLeavesBrowse(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter")) // suspend + navigate
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

// While a search is parked the footer key bar advertises esc → results (and
// still q → back, which leaves browse outright — the deliberate asymmetry),
// not the open-input "esc cancel" — the back affordance tracks the modal state.
func TestBrowseSearchSuspendedHeaderShowsResults(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter")) // suspend + navigate
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

// Restoring the search while the jumped-to directory listing is still in flight, then
// cancelling, must not leave isBrowseLoading stuck true. Enter starts an async load of
// the (unlisted) parent dir; esc restores the parked search and esc cancels it, both
// before the listing returns; the stale browseDirMsg is then dropped on its old
// generation. Without restoreBrowseSearch dropping the in-flight listing, isBrowseLoading
// would never clear and navigation would be paused forever.
func TestBrowseSearchRestoreWhileLoadingClearsLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/etc", "etc", true, 0), // a second top-level row so navigation can be proven
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	// Enter jumps to /home (never listed → a real async load). Capture the command but
	// do NOT deliver its browseDirMsg, so the listing stays in flight.
	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter on a match in an unlisted dir should start an async listing")
	}
	if !m.isBrowseLoading {
		t.Fatal("precondition: the jumped-to listing should be in flight (isBrowseLoading)")
	}
	stale := cmd().(browseDirMsg) // delivered last, after the generation has moved on

	// esc restores the parked search — and must drop the in-flight jump, clearing loading.
	m = update(t, m, press("esc"))
	if !m.browseSearching {
		t.Fatal("first esc should restore the search overlay")
	}
	if m.isBrowseLoading {
		t.Error("restoring search must drop the in-flight jump listing and clear isBrowseLoading")
	}

	// esc again cancels the restored search; the stale listing then returns and is
	// dropped on its old generation.
	m = update(t, m, press("esc"))
	if m.browseSearching {
		t.Fatal("second esc should cancel the restored search")
	}
	m = update(t, m, stale)
	if m.isBrowseLoading {
		t.Error("isBrowseLoading must stay cleared after the stale jump listing is dropped, not stuck true")
	}

	// Navigation is paused while isBrowseLoading; moving the cursor proves it is not frozen.
	m = update(t, m, press("j"))
	if m.browseCursor != 1 {
		t.Errorf("browse navigation should work after restore+cancel, cursor = %d want 1", m.browseCursor)
	}
}

// Restoring the search while the jump listing is in flight, then typing a new query
// (which supersedes that listing) and cancelling, must also not leave isBrowseLoading
// stuck — the new search never sets loading, so only restoreBrowseSearch clearing it
// keeps navigation alive.
func TestBrowseSearchRestoreThenTypeClearsLoading(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/etc", "etc", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
		bnode("/home/reportx.txt", "reportx.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report")

	next, cmd := m.Update(press("enter")) // jump to /home → async load, loading=true
	m = next.(Model)
	if cmd == nil || !m.isBrowseLoading {
		t.Fatal("precondition: enter should start an in-flight jump listing")
	}
	stale := cmd().(browseDirMsg)

	m = update(t, m, press("esc")) // restore (drops the jump, clears loading)
	if !m.browseSearching || m.isBrowseLoading {
		t.Fatalf("restore should reopen search and clear loading: searching=%v loading=%v", m.browseSearching, m.isBrowseLoading)
	}

	// Type a further character; the new search dispatches and its result lands.
	m = typeSearch(t, m, "x") // "report" -> "reportx"
	if m.isBrowseLoading {
		t.Error("a refired search must not set isBrowseLoading")
	}

	// The original jump listing finally returns and is dropped (old generation).
	m = update(t, m, stale)

	// Cancel and confirm navigation works.
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

// Enter pressed after editing the query but before the new scan returns must not
// accept a row from the previous query. The visible rows are kept between
// keystrokes (no per-edit flicker), so without a guard the stale row would stay
// selectable in that window; Enter is gated on the query that produced the rows.
func TestBrowseSearchEnterDropsStaleRowAfterEdit(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 10),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "report") // settle: the visible row belongs to "report"
	if len(m.browseSearchRows) != 1 || m.browseSearchShownQuery != "report" {
		t.Fatalf("precondition: 'report' should be the shown query with one row, got shown=%q rows=%+v",
			m.browseSearchShownQuery, m.browseSearchRows)
	}

	// Edit the query to "reportz" but do NOT deliver the new scan's result, so the
	// visible row still belongs to "report" while the live query has moved on.
	next, cmd := m.Update(press("z"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("editing the query should dispatch a new search")
	}
	if m.browseSearchQuery != "reportz" || m.browseSearchShownQuery != "report" {
		t.Fatalf("after edit: query=%q shown=%q, want reportz/report", m.browseSearchQuery, m.browseSearchShownQuery)
	}

	// Enter now, before the "reportz" result returns, must be a no-op: no navigation
	// command, and the search stays open so the user can keep typing.
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

	// Once the fresh "reportz" result (no match) arrives, the shown query catches up
	// to the live query so Enter is live again.
	m = update(t, m, cmd().(browseSearchMsg))
	if m.browseSearchShownQuery != "reportz" {
		t.Errorf("after the scan returns, shown query = %q, want reportz", m.browseSearchShownQuery)
	}
}

// Backspace trims the query and refires the search, broadening the matches.
func TestBrowseSearchBackspaceTrimsAndRefires(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/report.txt", "report.txt", false, 1),
		bnode("/home/rep.md", "rep.md", false, 1),
	)))
	m = openSearch(t, m)
	m = typeSearch(t, m, "repo") // 'o' narrows to report.txt only (rep.md has no 'o')
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

// Emptying the query (backspace to nothing) clears the results synchronously with
// no command — there is nothing to scan — while keeping the search input open.
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

	next, cmd := m.Update(press("backspace")) // "a" -> ""
	m = next.(Model)
	if cmd != nil {
		t.Error("emptying the query must clear synchronously, with no command")
	}
	// The no-scan contract: an empty query supersedes and clears in place; it must
	// never reach the store (no command above, and no extra Search call here).
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

// A whitespace-only query renders the empty "type to search" state, so Enter must
// behave exactly like an empty query: no selection → close the search and leave the
// listing untouched, never a stale no-op left over from the accept gate.
func TestBrowseSearchWhitespaceQueryEnterCloses(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t,
		bnode("/home", "home", true, 0),
		bnode("/home/a.txt", "a.txt", false, 1),
	)))
	dirBefore := m.browseDir
	m = openSearch(t, m)

	next, cmd := m.Update(press(" ")) // whitespace-only: clears synchronously, no scan
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

// While searching, the footer help reflects the search semantics — Enter opens the
// selected match and esc cancels — rather than the filter's borrowed "apply"/"clear"
// wording (esc exits search and restores the listing; it does not clear a query in
// place).
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

// ctrl+c is a hard quit from anywhere, including the search input.
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

// Keys that drive actions or navigation in normal browse (q quit/back, s shell, ?
// help, h parent, l open, and the j/k Up/Down letters) are all literal query
// characters while the search input is open — a fuzzy input must let text entry win
// so common searches like json/java/kernel are typable.
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

// Result navigation in search uses the arrows plus ctrl+k/ctrl+j (never plain
// k/j, which stay literal text): each moves the result cursor without editing the
// query, while a plain j/k edits the query and refires the live search.
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

	// Arrows and ctrl+j/ctrl+k move the result cursor, emit no command, and never
	// touch the query.
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

	// A plain k is NOT navigation: it is appended to the query and refires the search.
	next, cmd := m.Update(press("k"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("plain k should be typed into the query and refire the search")
	}
	if m.browseSearchQuery != "jobk" {
		t.Errorf("plain k should append to the query, got %q want jobk", m.browseSearchQuery)
	}
}

// clearBrowse zeroes every browseSearch* field: the search rows carry filenames,
// which must not linger in the model once browse is left (non-negotiable #1).
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
	// Park the search (as Enter would) so clearBrowse is exercised against the one
	// state where search data is meant to outlive the overlay — it must still be wiped.
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

// browseSearchSummary reports each search state for the body summary line.
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

// A broad query whose matches exceed the result cap returns the capped rows but the
// true total, and the footer reports "showing N of Total" so a truncated result
// never masquerades as complete.
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

// A store search error is shown in the footer while the search stays open, and the
// error never carries a filename (path-free invariant).
func TestBrowseSearchErrorVisibleAndPathFree(t *testing.T) {
	a, store := browseAppWithStore(t,
		bnode("/home", "home", true, 0),
		bnode("/home/topsecret.txt", "topsecret.txt", false, 1),
	)
	m := openBrowse(t, newTestModel(t, a))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = openSearch(t, m)
	store.failSearches(errors.New("browse store unavailable"))
	// Query "secret" against the file "topsecret.txt": the footer echoes the user's
	// query ("/secret"), which is fine, but the filename ("topsecret") must never
	// appear — a store error is path-free.
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

// Search results render each match's full path (not just its bare name) under a
// "Path" column header, and an overlong path is clipped to the table width rather
// than wrapping.
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
	hdr := lineContaining(t, wide, "Size") // the table header row
	if !strings.Contains(hdr, "Path") {
		t.Errorf("the search list flex column should be labelled 'Path', got %q", hdr)
	}

	// Narrow terminal: the path is clipped (ellipsis) but keeps the basename —
	// the part that identifies the match — and the row stays within the table
	// width on a single line, never wrapped.
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

// The browse footer key bar advertises "q back" normally, but while the search
// input is open q is literal text and esc cancels — so the bar must switch to
// "esc cancel" rather than keep a contradictory affordance.
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

// The browse footer advertises search and the help overlay documents it, so the
// feature is discoverable from the browse view.
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
