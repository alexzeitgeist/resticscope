package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
