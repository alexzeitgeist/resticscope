package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// --- browse test helpers ---

func bnode(p, name string, isDir bool, size int64) model.BrowseNode {
	return model.BrowseNode{Path: p, Name: name, IsDir: isDir, Size: size}
}

func browseScan(reason model.PartialReason, frontier model.BrowseFrontier, nodes ...model.BrowseNode) model.BrowseScan {
	return model.BrowseScan{Nodes: nodes, Reason: reason, LoadedEntries: len(nodes), Frontier: frontier}
}

// browseApp builds a detail app whose restic returns the given scan from
// ListSnapshotTree, with browse caps that leave room for one load-more (initial
// caps below their session ceilings).
func browseApp(t *testing.T, scan model.BrowseScan) *app.App {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{
		MaxEntries:          2,
		MaxJSONBytes:        1024,
		Timeout:             config.Duration(time.Second),
		MaxSessionEntries:   8,
		MaxSessionJSONBytes: 8192,
	}
	a.Restic = stubRestic{scan: scan}
	return a
}

// openBrowse drives the model into browseView with a loaded tree: enter detail,
// press b, then run the browse command and deliver its result.
func openBrowse(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("b produced no browse command")
	}
	msg, ok := cmd().(browseLoadedMsg)
	if !ok {
		t.Fatalf("browse command produced %T, want browseLoadedMsg", cmd())
	}
	return update(t, m, msg)
}

// --- tests ---

// A complete scan opens the browse view showing the tree, and the footer does not
// advertise load-more (a complete tree has nothing more to fetch).
func TestBrowseCompleteEntersViewWithoutLoadMore(t *testing.T) {
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/home", "home", true, 0),
		bnode("/home/alex", "alex", true, 0),
		bnode("/home/alex/f.txt", "f.txt", false, 42),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if m.browseLoading || m.browsing {
		t.Errorf("load flags should be clear after the result arrives: loading=%v browsing=%v", m.browseLoading, m.browsing)
	}
	view := m.View().Content
	if !strings.Contains(view, "home") {
		t.Errorf("browse view missing the listed directory\n---\n%s", view)
	}
	if footer := stripANSI(m.footerView()); strings.Contains(footer, "load more") {
		t.Errorf("complete tree should not advertise load more\n---\n%s", footer)
	}
}

// An entry-cap partial below the session ceiling advertises load-more and marks
// the truncated tree as partial (never as complete).
func TestBrowseEntryCapPartialOffersLoadMore(t *testing.T) {
	scan := browseScan(model.PartialEntryCap, model.BrowseFrontier{Path: "/b", IsDir: true},
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if footer := stripANSI(m.footerView()); !strings.Contains(footer, "load more") {
		t.Errorf("entry-cap partial below ceiling should advertise load more\n---\n%s", footer)
	}
	if view := stripANSI(m.View().Content); !strings.Contains(view, "partial: entry cap") {
		t.Errorf("browse header should mark the tree partial\n---\n%s", view)
	}
}

// A timeout partial is first-class and usable, but timeout caps can never be
// raised, so load-more is hidden even with at least one node.
func TestBrowseTimeoutPartialHidesLoadMore(t *testing.T) {
	scan := browseScan(model.PartialTimeout, model.BrowseFrontier{Path: "/a", IsDir: true},
		bnode("/a", "a", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

	if footer := stripANSI(m.footerView()); strings.Contains(footer, "load more") {
		t.Errorf("timeout partial must not advertise load more\n---\n%s", footer)
	}
	if view := stripANSI(m.View().Content); !strings.Contains(view, "partial: timeout") {
		t.Errorf("browse header should mark the tree a timeout partial\n---\n%s", view)
	}
}

// When the entry cap is already at the session ceiling, an entry-cap partial can
// load no more, so the footer hides load-more and shows shell/back guidance.
func TestBrowseAtCeilingHidesLoadMore(t *testing.T) {
	a := browseApp(t, browseScan(model.PartialEntryCap, model.BrowseFrontier{Path: "/b", IsDir: true},
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	))
	a.Cfg.Browse.MaxEntries = 8 // already at the session ceiling
	m := openBrowse(t, newTestModel(t, a))

	footer := stripANSI(m.footerView())
	if strings.Contains(footer, "load more") {
		t.Errorf("at-ceiling partial must not advertise load more\n---\n%s", footer)
	}
	if !strings.Contains(footer, "shell") || !strings.Contains(footer, "back") {
		t.Errorf("footer should still show shell/back guidance\n---\n%s", footer)
	}
}

// Load-more keeps the old tree visible while the reload runs, then replaces it and
// restores the current directory and selection by path.
func TestBrowseLoadMoreReplacesTreeAndRestoresSelection(t *testing.T) {
	scan := browseScan(model.PartialEntryCap, model.BrowseFrontier{Path: "/b", IsDir: true},
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

	m = update(t, m, press("j")) // select /b (the second child)
	if got := m.selectedBrowsePath(); got != "/b" {
		t.Fatalf("selection = %q, want /b", got)
	}

	next, cmd := m.Update(press("r")) // load more
	m = next.(Model)
	if !m.browseLoading {
		t.Fatal("load-more should set browseLoading")
	}
	if m.browseResult == nil || m.browseResult.LoadedEntries != 2 {
		t.Fatalf("old tree must stay visible during load-more, got %+v", m.browseResult)
	}
	if cmd == nil {
		t.Fatal("load-more produced no command")
	}

	// Deliver a richer, complete result tagged with the current (advanced) gen,
	// carrying the doubled caps a real load-more would have run with.
	bigger := model.BuildBrowseTree(browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
		bnode("/z", "z", true, 0),
	), model.NextBrowseLimits(m.browseResult.Limits))
	m = update(t, m, browseLoadedMsg{gen: m.browseGen, result: &bigger})

	if m.browseLoading {
		t.Error("browseLoading should clear when the load-more result arrives")
	}
	if m.browseResult.LoadedEntries != 3 {
		t.Errorf("tree not replaced: LoadedEntries = %d, want 3", m.browseResult.LoadedEntries)
	}
	if got := m.selectedBrowsePath(); got != "/b" {
		t.Errorf("selection not restored by path: got %q, want /b", got)
	}
	if view := m.View().Content; !strings.Contains(view, "z/") {
		t.Errorf("replaced tree should show the newly loaded entry\n---\n%s", view)
	}
}

// Back (q and esc) while a load-more is in flight cancels just that load and stays
// in browse with the old tree — it does not leave the view or clear state.
func TestBrowseBackDuringLoadMoreCancelsAndKeepsTree(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		t.Run(k, func(t *testing.T) {
			scan := browseScan(model.PartialEntryCap, model.BrowseFrontier{Path: "/b", IsDir: true},
				bnode("/a", "a", true, 0),
				bnode("/b", "b", true, 0),
			)
			m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
			genBefore := m.browseGen

			next, _ := m.Update(press("r")) // start load-more
			m = next.(Model)
			if !m.browseLoading {
				t.Fatal("precondition: load-more should be in flight")
			}

			next, cmd := m.Update(press(k))
			m = next.(Model)
			if m.view != browseView {
				t.Errorf("%q during load-more should stay in browse, view = %d", k, m.view)
			}
			if m.browseResult == nil || m.browseResult.LoadedEntries != 2 {
				t.Errorf("%q during load-more should keep the old tree, got %+v", k, m.browseResult)
			}
			if m.browseResult.Limits.MaxEntries != 2 {
				t.Errorf("%q must not advance the visible tree's committed caps: MaxEntries = %d, want 2", k, m.browseResult.Limits.MaxEntries)
			}
			if !m.browseCanLoadMore() {
				t.Errorf("%q during load-more should leave load-more available (old tree still partial)", k)
			}
			if m.browseLoading || m.browsing {
				t.Errorf("%q should clear the load flags: loading=%v browsing=%v", k, m.browseLoading, m.browsing)
			}
			if m.browseGen <= genBefore+1 {
				t.Errorf("%q should advance the generation past the cancelled load (gen=%d)", k, m.browseGen)
			}
			if cmd != nil {
				t.Errorf("%q during load-more should emit no command", k)
			}
		})
	}
}

// A failed load-more keeps the old tree visible without advancing the committed
// caps: the visible tree's limits are unchanged, the footer still offers load-more
// (doubling from those limits, not a phantom intermediate level), and leaving
// browse clears the transient error so it never lingers in the detail view.
func TestBrowseLoadMoreErrorKeepsTreeAndLimits(t *testing.T) {
	scan := browseScan(model.PartialEntryCap, model.BrowseFrontier{Path: "/b", IsDir: true},
		bnode("/a", "a", true, 0),
		bnode("/b", "b", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
	if m.browseResult.Limits.MaxEntries != 2 {
		t.Fatalf("precondition: initial MaxEntries = %d, want 2", m.browseResult.Limits.MaxEntries)
	}

	next, _ := m.Update(press("r")) // start load-more
	m = next.(Model)
	if !m.browseLoading {
		t.Fatal("precondition: load-more should be in flight")
	}

	m = update(t, m, browseLoadedMsg{gen: m.browseGen, err: errors.New("ls failed")})

	if m.view != browseView {
		t.Errorf("failed load-more should stay in browse, view = %d", m.view)
	}
	if m.browseResult == nil || m.browseResult.LoadedEntries != 2 {
		t.Errorf("failed load-more should keep the old tree, got %+v", m.browseResult)
	}
	if m.browseResult.Limits.MaxEntries != 2 {
		t.Errorf("failed load-more must not advance committed caps: MaxEntries = %d, want 2", m.browseResult.Limits.MaxEntries)
	}
	if !m.browseCanLoadMore() {
		t.Error("footer should still offer load-more after a failed load-more")
	}
	if m.statusMsg == "" {
		t.Fatal("a failed load-more should surface a status message")
	}

	// Backing out clears the transient browse error so it never lingers in detail.
	next, _ = m.Update(press("q"))
	m = next.(Model)
	if m.view != detailView {
		t.Errorf("q after a failed load-more should return to detail, view = %d", m.view)
	}
	if m.statusMsg != "" {
		t.Errorf("leaving browse should clear the transient browse error, got %q", m.statusMsg)
	}
}

// A late browseLoadedMsg whose generation no longer matches (a cancelled or
// superseded load) is silently discarded and never resurrects browse state.
func TestBrowseStaleGenerationDiscarded(t *testing.T) {
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/a", "a", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
	before := m.browseResult

	other := model.BuildBrowseTree(browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/zzz", "zzz", true, 0),
	), m.browseResult.Limits)
	m = update(t, m, browseLoadedMsg{gen: m.browseGen + 99, result: &other})

	if m.browseResult != before {
		t.Error("a stale-generation result must be discarded, not applied")
	}
	if view := m.View().Content; strings.Contains(view, "zzz") {
		t.Errorf("stale result leaked into the view\n---\n%s", view)
	}
}

// Leaving browse with no load in flight returns to the detail view and clears all
// session browse state, so no filename/path data lingers in the model.
func TestBrowseLeaveClearsSessionState(t *testing.T) {
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/secret-dir", "secret-dir", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
	if !strings.Contains(m.View().Content, "secret-dir") {
		t.Fatal("precondition: browse view should show the loaded entry")
	}

	m = update(t, m, press("q"))
	if m.view != detailView {
		t.Errorf("q from a loaded browse should return to detail, view = %d", m.view)
	}
	if m.browseResult != nil || m.browseRepo != "" || m.browseSnapshot != "" || m.browseDir != "" {
		t.Errorf("leaving browse should clear session state, got result=%v repo=%q snap=%q dir=%q",
			m.browseResult, m.browseRepo, m.browseSnapshot, m.browseDir)
	}
}

// At a wide width the browse table shows every metadata column (Modified, Perms,
// Owner) with their values; narrowing the terminal drops Owner, then Perms, then
// Modified in that priority order while Name and Size always remain.
func TestBrowseRendersMetadataColumnsResponsively(t *testing.T) {
	mod := time.Date(2026, 5, 26, 11, 28, 0, 0, time.UTC)
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

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
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		model.BrowseNode{Path: "/file.txt", Name: "file.txt", Size: 42, ModTime: mod, Permissions: "-rw-r--r--", UID: 1000, GID: 1000, OwnerKnown: true},
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
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
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		model.BrowseNode{Path: "/wide﹕name.txt", Name: "wide﹕name.txt", Size: 42, ModTime: mod, Permissions: "-rw-rw----", UID: 10316, GID: 1023, OwnerKnown: true},
		model.BrowseNode{Path: "/plain.txt", Name: "plain.txt", Size: 42, ModTime: mod, Permissions: "-rw-rw----", UID: 10316, GID: 1023, OwnerKnown: true},
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
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
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		model.BrowseNode{Path: "/root.txt", Name: "root.txt", Size: 1, ModTime: mod, Permissions: "-rw-------", UID: 0, GID: 0, OwnerKnown: true},
		model.BrowseNode{Path: "/anon.txt", Name: "anon.txt", Size: 1, ModTime: mod, Permissions: "-rw-r--r--"},
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
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

// Right arrow and l are aliases for Enter in browse: each opens the selected
// directory. On a file they are a no-op (files have no open action).
func TestBrowseRightArrowOpensDirectory(t *testing.T) {
	for _, k := range []string{"right", "l"} {
		t.Run(k, func(t *testing.T) {
			scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
				bnode("/dir", "dir", true, 0),
				bnode("/dir/child.txt", "child.txt", false, 5),
			)
			m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
			if m.browseDir != "/" {
				t.Fatalf("precondition: browseDir = %q, want /", m.browseDir)
			}
			m = update(t, m, press(k)) // cursor is on /dir
			if m.browseDir != "/dir" {
				t.Errorf("%q should open the selected directory, browseDir = %q", k, m.browseDir)
			}
		})
	}
}

func TestBrowseRightArrowOnFileIsNoOp(t *testing.T) {
	for _, k := range []string{"right", "l"} {
		t.Run(k, func(t *testing.T) {
			scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
				bnode("/a.txt", "a.txt", false, 5),
			)
			m := openBrowse(t, newTestModel(t, browseApp(t, scan)))
			m = update(t, m, press(k)) // cursor is on the file
			if m.browseDir != "/" {
				t.Errorf("%q on a file should be a no-op, browseDir = %q", k, m.browseDir)
			}
		})
	}
}

// The help overlay's Browse section documents the open alias as enter/→/l, while
// the compact browse footer must not advertise the right-arrow alias.
func TestBrowseHelpDocumentsOpenAliasFooterHidesIt(t *testing.T) {
	scan := browseScan(model.BrowseComplete, model.BrowseFrontier{},
		bnode("/dir", "dir", true, 0),
	)
	m := openBrowse(t, newTestModel(t, browseApp(t, scan)))

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

// Quitting (ctrl+c) while a browse load is in flight cancels the browse context so
// the restic subprocess does not outlive the UI.
func TestBrowseQuitCancelsInFlightLoad(t *testing.T) {
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{MaxEntries: 2, MaxJSONBytes: 1024, Timeout: config.Duration(time.Minute), MaxSessionEntries: 8, MaxSessionJSONBytes: 8192}
	started := make(chan struct{})
	a.Restic = blockingRestic{started: started}
	m := newTestModel(t, a)

	m = update(t, m, press("enter"))
	next, cmd := m.Update(press("b"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected a browse command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("browse never reached restic")
	}

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // hard quit cancels m.ctx -> browse ctx

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("browse did not unblock after quit cancelled the context")
	}
}
