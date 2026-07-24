package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/theme"

	tea "charm.land/bubbletea/v2"
)

// These tests cover version-query state, privacy, cancellation, and host recovery.

// findApp builds a complete in-memory browse-to-find flow and exposes call capture.
func findApp(t *testing.T, results []model.FindSnapshotResult, nodes ...model.BrowseNode) (*app.App, *stubFindCapture) {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	cap := &stubFindCapture{}
	a.Restic = stubRestic{browseNodes: nodes, findResults: results, findCap: cap}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func(context.Context) (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a, cap
}

// drivePastFind synchronously delivers a find result.
func drivePastFind(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a find-versions command")
	}
	msg, ok := cmd().(findVersionsMsg)
	if !ok {
		t.Fatalf("find-versions command produced %T, want findVersionsMsg", cmd())
	}
	return update(t, m, msg)
}

// openFindVersions opens a selected file's populated versions table.
func openFindVersions(t *testing.T, m Model, fileName string) Model {
	t.Helper()
	m = openBrowse(t, m)
	for i, r := range m.browseRows {
		if r.Name == fileName {
			m.browseCursor = i
			break
		}
	}
	next, cmd := m.Update(press("v"))
	m = next.(Model)
	return drivePastFind(t, m, cmd)
}

func mkFindResults(snapID string, size int64, mtime time.Time) []model.FindSnapshotResult {
	return []model.FindSnapshotResult{
		{SnapshotID: snapID, Hits: 1, Matches: []model.FindMatch{
			{Path: "/hostname", Size: size, ModTime: mtime, Permissions: "-rw-r--r--"},
		}},
	}
}

// detailAppSnapID returns a seeded snapshot for metadata joins.
func detailAppSnapID(t *testing.T, m Model) string {
	t.Helper()
	for _, r := range m.rows {
		if r.Name == "repo-a" && len(r.State.Snapshots) > 0 {
			return r.State.Snapshots[0].ID
		}
	}
	t.Fatal("no repo-a snapshot id available")
	return ""
}

// Opening versions runs once and populates grouped rows.
func TestFindVersionsOpensFromBrowseAndPopulates(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	snapID := detailAppSnapID(t, m)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findResults: mkFindResults(snapID, 12, mt),
		findCap:     cap,
	}

	m = openFindVersions(t, m, "hostname")
	if m.view != findVersionsView {
		t.Fatalf("view = %d, want findVersionsView", m.view)
	}
	if m.findLoading() {
		t.Error("after the result lands, loading should be false")
	}
	if len(m.findRows) != 1 {
		t.Fatalf("findRows = %d, want one distinct version", len(m.findRows))
	}
	if m.findRows[0].Size != 12 {
		t.Errorf("findRows[0].Size = %d, want 12", m.findRows[0].Size)
	}
	calls, host, pattern := cap.snapshot()
	if calls != 1 {
		t.Errorf("FindMatches calls = %d, want exactly 1", calls)
	}
	if pattern != "/hostname" {
		t.Errorf("pattern = %q, want /hostname", pattern)
	}
	if host == "" {
		t.Error("default-narrow host filter must pass a non-empty host")
	}
	if m.findResultHost != host {
		t.Errorf("findResultHost = %q, want %q (the host the call actually used)", m.findResultHost, host)
	}
	if m.findResultAllHosts {
		t.Error("findResultAllHosts should reflect the request: false")
	}
}

// Directories remain in browse with regular-file guidance.
func TestFindVersionsKeyOnDirectoryShowsMessage(t *testing.T) {
	a, _ := findApp(t, nil, bnode("/dir", "dir", true, 0))
	m := newTestModel(t, a)
	m = openBrowse(t, m)
	next, cmd := m.Update(press("v"))
	m = next.(Model)
	if cmd != nil {
		t.Error("v on a directory should emit no command")
	}
	if m.view != browseView {
		t.Errorf("v on a directory should leave the view in browse, got %d", m.view)
	}
	if m.statusMsg != "" {
		t.Errorf("statusMsg = %q, want no global footer notice", m.statusMsg)
	}
	if got := m.browseSummaryLine(); got != "versions: select a regular file" {
		t.Errorf("browseSummaryLine() = %q, want directory guidance", got)
	}
}

func TestFindVersionsDirectoryNoticeClearsOnBrowseNavigation(t *testing.T) {
	a, _ := findApp(t, nil, bnode("/run", "run", true, 0))
	m := newTestModel(t, a)
	m = openBrowse(t, m)
	m = update(t, m, press("v"))
	if got := m.browseSummaryLine(); got != "versions: select a regular file" {
		t.Fatalf("precondition: browseSummaryLine() = %q, want directory guidance", got)
	}

	m = pressBrowse(t, m, "enter")
	if m.browseDir != "/run" {
		t.Fatalf("browseDir = %q, want /run", m.browseDir)
	}
	if got := m.browseSummaryLine(); got == "versions: select a regular file" {
		t.Fatalf("directory guidance should clear after navigation, got %q", got)
	}
}

// The host toggle reruns the query across all hosts.
func TestFindVersionsAToggleRerunsWithAllHosts(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	snapID := detailAppSnapID(t, m)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findResults: mkFindResults(snapID, 12, mt),
		findCap:     cap,
	}
	m = openFindVersions(t, m, "hostname")
	if calls, _, _ := cap.snapshot(); calls != 1 {
		t.Fatalf("precondition: one find call, got %d", calls)
	}

	next, cmd := m.Update(press("a"))
	m = next.(Model)
	if !m.findRequestAllHosts {
		t.Error("`a` should flip the user-toggle to all-hosts")
	}
	m = drivePastFind(t, m, cmd)

	calls, host, _ := cap.snapshot()
	if calls != 2 {
		t.Errorf("`a` should trigger a second find call, got calls=%d", calls)
	}
	if host != "" {
		t.Errorf("toggled all-hosts call should pass empty host, got %q", host)
	}
	if !m.findResultAllHosts || m.findResultHost != "" {
		t.Errorf("result fields should reflect all-hosts response: AllHosts=%v Host=%q",
			m.findResultAllHosts, m.findResultHost)
	}
}

// Pending host toggles cannot relabel rows from the previous result.
func TestFindVersionsHostLabelDrivenByResultNotRequest(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	snapID := detailAppSnapID(t, m)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findResults: mkFindResults(snapID, 12, mt),
		findCap:     cap,
	}
	m = openFindVersions(t, m, "hostname")
	hostBefore := m.findResultHost
	if hostBefore == "" {
		t.Fatal("precondition: a default-narrow result must record a host")
	}

	next, _ := m.Update(press("a"))
	m = next.(Model)
	if !m.findRequestAllHosts {
		t.Fatal("precondition: `a` should flip the user toggle")
	}
	if got := m.findHostLabel(); got != hostBefore {
		t.Errorf("mid-reload host label = %q, want %q (the prior result's host)", got, hostBefore)
	}
}

// q and escape both return to browse.
func TestFindVersionsBackKeys(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		t.Run(k, func(t *testing.T) {
			mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
			a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
			m := newTestModel(t, a)
			snapID := detailAppSnapID(t, m)
			a.Restic = stubRestic{
				browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
				findResults: mkFindResults(snapID, 12, mt),
				findCap:     cap,
			}
			m = openFindVersions(t, m, "hostname")
			browseDirBefore, browseCursorBefore := m.browseDir, m.browseCursor
			genBefore := m.findGen

			next, _ := m.Update(press(k))
			m = next.(Model)
			if m.view != browseView {
				t.Errorf("%q should return to browse, view = %d", k, m.view)
			}
			if m.browseDir != browseDirBefore || m.browseCursor != browseCursorBefore {
				t.Errorf("%q should preserve prior browse state: dir=%q cursor=%d (was %q,%d)",
					k, m.browseDir, m.browseCursor, browseDirBefore, browseCursorBefore)
			}
			if m.findPath != "" || m.findRepo != "" || m.findOriginHost != "" || m.findRows != nil {
				t.Errorf("%q should clear find state: path=%q repo=%q host=%q rows=%v",
					k, m.findPath, m.findRepo, m.findOriginHost, m.findRows)
			}
			if m.findResultHost != "" || m.findResultAllHosts {
				t.Errorf("%q should clear result host fields: host=%q allHosts=%v",
					k, m.findResultHost, m.findResultAllHosts)
			}
			if m.findGen <= genBefore {
				t.Errorf("%q should advance the find generation so a racing msg is dropped (gen=%d, was %d)",
					k, m.findGen, genBefore)
			}
		})
	}
}

// Late results cannot resurrect cleared version state.
func TestFindVersionsStaleMessageDropped(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	snapID := detailAppSnapID(t, m)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findResults: mkFindResults(snapID, 12, mt),
		findCap:     cap,
	}
	m = openFindVersions(t, m, "hostname")
	m = update(t, m, press("q"))
	staleGen := m.findGen - 1

	stale := findVersionsMsg{gen: staleGen, result: app.FindFileVersionsResult{
		Host: "h", Rows: []model.FileVersion{{Size: 99}},
	}}
	m = update(t, m, stale)
	if m.view != browseView {
		t.Errorf("stale msg flipped view to %d, want browseView", m.view)
	}
	if m.findRows != nil {
		t.Errorf("stale msg resurrected find rows: %+v", m.findRows)
	}
}

// Unknown hosts offer recovery through an all-host query.
func TestFindVersionsUnknownHostErrAndRecovery(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findResults: mkFindResults("unused", 12, mt),
		findCap:     cap,
	}
	m = openBrowse(t, m)
	for i, r := range m.browseRows {
		if r.Name == "hostname" {
			m.browseCursor = i
			break
		}
	}
	// An untracked snapshot leaves originHost empty and triggers unknown-host recovery.
	m.browseSnapshot = "00000000000000000000000000000000"
	next, cmd := m.Update(press("v"))
	m = next.(Model)
	m = drivePastFind(t, m, cmd)

	if len(m.findRows) != 0 {
		t.Errorf("findRows should be empty on ErrFindUnknownHost, got %+v", m.findRows)
	}
	if !strings.Contains(m.findErr, "press a to search all hosts") {
		t.Errorf("findErr should advertise the `a` recovery: %q", m.findErr)
	}
	next, cmd = m.Update(press("a"))
	m = next.(Model)
	m = drivePastFind(t, m, cmd)
	calls, host, _ := cap.snapshot()
	if calls != 1 {
		t.Errorf("recovery call count = %d, want 1 (the first attempt didn't reach restic)", calls)
	}
	if host != "" {
		t.Errorf("recovery should pass empty host, got %q", host)
	}
	if !m.findResultAllHosts {
		t.Error("recovery response should set result.AllHosts=true")
	}
}

// Restic errors render as one path-free line.
func TestFindVersionsResticErrorSurfaces(t *testing.T) {
	a, _ := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findErr:     errors.New("repository is locked\nextra"),
	}
	m = openFindVersions(t, m, "hostname")
	if m.findErr == "" || !strings.HasPrefix(m.findErr, "versions: ") {
		t.Errorf("findErr should be prefixed 'versions: ', got %q", m.findErr)
	}
	if strings.Contains(m.findErr, "\n") {
		t.Errorf("findErr must be a single line, got %q", m.findErr)
	}
}

func TestApplyFindVersionsMsgDropsStaleGen(t *testing.T) {
	m := Model{findGen: 5}
	in := findVersionsMsg{gen: 4, result: app.FindFileVersionsResult{Rows: []model.FileVersion{{Size: 1}}}}
	out := m.applyFindVersionsMsg(in)
	if out.findRows != nil {
		t.Errorf("stale msg applied: rows=%+v", out.findRows)
	}
}

func TestFindVersionsCursorPausedWhileLoading(t *testing.T) {
	m := Model{view: findVersionsView, findCancel: func() {}, findCursor: 0}
	m.keys = defaultKeys()
	m.ctx = t.Context()
	next, _ := m.handleFindVersionsKey(press("j"))
	if next.(Model).findCursor != 0 {
		t.Errorf("cursor moved while loading: cursor = %d, want 0", next.(Model).findCursor)
	}
}

func TestStartFindVersionsCancelsPriorFindBeforeClearing(t *testing.T) {
	canceled := false
	m := Model{
		ctx:        t.Context(),
		findGen:    7,
		findCancel: func() { canceled = true },
		findRows:   []model.FileVersion{{Size: 1}},
	}

	next, cmd := m.startFindVersions("repo-a", "host-a", "/x")
	if !canceled {
		t.Fatal("startFindVersions should cancel an existing in-flight find before clearing state")
	}
	if cmd == nil {
		t.Fatal("startFindVersions should dispatch a find command")
	}
	if next.findGen != 8 {
		t.Fatalf("findGen = %d, want one supersede to 8", next.findGen)
	}
	if !next.findLoading() {
		t.Fatal("new find should be in flight with a fresh cancel func")
	}
	if next.findRepo != "repo-a" || next.findOriginHost != "host-a" || next.findPath != "/x" {
		t.Fatalf("new find query not pinned: repo=%q host=%q path=%q",
			next.findRepo, next.findOriginHost, next.findPath)
	}
	if next.findRows != nil {
		t.Fatalf("old rows should be cleared before new find result lands: %+v", next.findRows)
	}
}

func TestPathLineDoesNotExpandSpacesInsidePath(t *testing.T) {
	m := Model{styles: newStyles(theme.Default())}
	p := "/E/OneDrive/Bilder/Eigene Aufnahmen/2026/05/20260503_132847286_iOS.jpg"

	line := stripANSI(m.pathLine("Path", p, 160))
	if !strings.Contains(line, "Eigene Aufnahmen") {
		t.Fatalf("path line should preserve the literal single space inside the path, got %q", line)
	}
}

// Special nodes cannot enter versions because extraction requires regular-file attestation.
func TestFindVersionsKeyOnNonFileShowsMessage(t *testing.T) {
	for _, typ := range []string{"symlink", "socket", "fifo", "dev"} {
		t.Run(typ, func(t *testing.T) {
			a, _ := findApp(t, nil, model.BrowseNode{Path: "/thing", Name: "thing", Type: typ})
			m := newTestModel(t, a)
			m = openBrowse(t, m)
			next, cmd := m.Update(press("v"))
			m = next.(Model)
			if cmd != nil {
				t.Errorf("v on a %s should emit no command", typ)
			}
			if m.view != browseView {
				t.Errorf("v on a %s should stay in browse, got %d", typ, m.view)
			}
			if got := m.browseSummaryLine(); got != "versions: select a regular file" {
				t.Errorf("browseSummaryLine() = %q, want regular-file guidance", got)
			}
		})
	}
}

// Singular version and snapshot counts remain grammatical.
func TestFindSummaryLineSingularCounts(t *testing.T) {
	m := Model{
		findRows: []model.FileVersion{{
			Occurrences: []model.FileVersionOccurrence{{SnapshotID: "a", Hostname: "pve"}},
		}},
		findResultHost: "pve",
	}
	if got, want := m.findSummaryLine(), "1 version across 1 snapshot · host: pve"; got != want {
		t.Errorf("findSummaryLine() = %q, want %q", got, want)
	}
}
