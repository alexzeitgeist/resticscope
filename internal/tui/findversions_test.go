package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// findversions_test.go drives the find-versions view through its state
// transitions: open from browse, message arrival, cursor moves, host toggle,
// both back keys, the path-free no-persistence discipline on leave, and the
// ErrFindUnknownHost recovery path.

// findApp wires a browse-capable app whose restic returns the given
// find-versions snapshot results when FindMatches is called. The browse path
// (stream → index → list) uses the supplied nodes, so `v` from browse can
// reach the find-versions view with a file selected. The find capture is
// returned so a test can assert host/pattern/call count.
func findApp(t *testing.T, results []model.FindSnapshotResult, nodes ...model.BrowseNode) (*app.App, *stubFindCapture) {
	t.Helper()
	a := detailApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	cap := &stubFindCapture{}
	a.Restic = stubRestic{browseNodes: nodes, findResults: results, findCap: cap}
	store := newFakeBrowseStore()
	a.Browse = app.NewBrowseSession(func() (app.BrowseStore, error) { return store, nil })
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a, cap
}

// drivePastFind delivers a synchronously-produced findVersionsMsg from the
// startFindVersions command, so the model lands on a populated find-versions
// view in one step (rather than waiting on a goroutine).
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

// openFindVersions drives the model into findVersionsView with the row table
// populated: open browse, place the cursor on a file, press `v`, deliver the
// resulting findVersionsMsg.
func openFindVersions(t *testing.T, m Model, fileName string) Model {
	t.Helper()
	m = openBrowse(t, m)
	// Move the cursor onto the chosen file (rows from openBrowse start at the
	// top of the root listing; the test setups put exactly one file at root,
	// so cursor 0 lands on it).
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

// detailAppSnapID picks a snapshot id from detailApp's seeded rows so the find
// query can join cached snapshot metadata. detailApp seeds three snapshots on
// repo-a; we use the first row's first snapshot.
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

// v on a file opens the find-versions view, runs the one find call, lands a
// row table populated from GroupFileVersions, and clears any loading/error.
func TestFindVersionsOpensFromBrowseAndPopulates(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, cap := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	// Seed find results keyed to repo-a's first snapshot id so the grouper
	// joins in the cached snapshot metadata.
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
	if m.findLoading {
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

// v on a directory entry is a no-op (directories have no version concept).
func TestFindVersionsKeyOnDirectoryIsNoOp(t *testing.T) {
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
}

// `a` toggles the host filter and re-runs the find with the new flag. The
// stub records the latest call's host arg, which must now be empty.
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

// The renderer reads only the result-of-record fields, never the user-toggle.
// Pressing `a` while a prior result is on screen must NOT relabel the visible
// rows until the new result lands.
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

	// Press `a`, but DO NOT deliver the new findVersionsMsg yet.
	next, _ := m.Update(press("a"))
	m = next.(Model)
	if !m.findRequestAllHosts {
		t.Fatal("precondition: `a` should flip the user toggle")
	}
	// While loading, the header still shows the prior result's host so the
	// rows on screen are never mislabeled mid-reload.
	if got := m.findHostLabel(); got != hostBefore {
		t.Errorf("mid-reload host label = %q, want %q (the prior result's host)", got, hostBefore)
	}
}

// Both q and esc return to browseView (the global Quit branch catches q; the
// view's own handler catches esc). Both must work — wiring only one is a bug.
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
			// Browse state is preserved across the round trip.
			if m.browseDir != browseDirBefore || m.browseCursor != browseCursorBefore {
				t.Errorf("%q should preserve prior browse state: dir=%q cursor=%d (was %q,%d)",
					k, m.browseDir, m.browseCursor, browseDirBefore, browseCursorBefore)
			}
			// All find state is cleared on leave so no filename lingers.
			if m.findPath != "" || m.findRepo != "" || m.findSnapshot != "" || m.findRows != nil {
				t.Errorf("%q should clear find state: path=%q repo=%q snap=%q rows=%v",
					k, m.findPath, m.findRepo, m.findSnapshot, m.findRows)
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

// A late findVersionsMsg whose generation no longer matches (e.g. the user
// pressed back, or toggled host, before the response arrived) is silently
// discarded and never resurrects find state.
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
	m = update(t, m, press("q")) // leave find-versions, advancing gen
	staleGen := m.findGen - 1

	// A stale message must not restore rows or flip the view back to versions.
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

// ErrFindUnknownHost surfaces the recovery affordance in the status line and
// leaves the rows empty; `a` then re-fires with allHosts=true.
func TestFindVersionsUnknownHostErrAndRecovery(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// fakeFindApp wraps the App so FindFileVersions can return the sentinel
	// without setting up an unknown-host repo state on the real App. Reuse the
	// real App but inject a wrapper Restic that triggers the path via an
	// untracked snapshot id.
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
	// Forcibly clobber the model's findSnapshot to one not in the cached
	// state so hostnameOf returns "" and FindFileVersions returns
	// ErrFindUnknownHost. startFindVersions reads m.browseSnapshot, so
	// override that instead.
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
	// `a` re-fires with allHosts=true; this time the find succeeds because
	// the snapshot id is irrelevant (allHosts skips hostnameOf entirely).
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

// Restic errors land as the path-free findErr first line.
func TestFindVersionsResticErrorSurfaces(t *testing.T) {
	a, _ := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	m := newTestModel(t, a)
	a.Restic = stubRestic{
		browseNodes: []model.BrowseNode{bnode("/hostname", "hostname", false, 12)},
		findErr:     errors.New("repository is locked\nextra"),
	}
	m = openFindVersions(t, m, "hostname")
	if m.findErr == "" || !strings.HasPrefix(m.findErr, "find: ") {
		t.Errorf("findErr should be prefixed 'find: ', got %q", m.findErr)
	}
	if strings.Contains(m.findErr, "\n") {
		t.Errorf("findErr must be a single line, got %q", m.findErr)
	}
}

// applyFindVersionsMsg drops a stale message even outside the back path: an
// independent unit test on the controller, so the gen-check rule is locked in
// without depending on the full open/back round trip.
func TestApplyFindVersionsMsgDropsStaleGen(t *testing.T) {
	m := Model{findGen: 5}
	in := findVersionsMsg{gen: 4, result: app.FindFileVersionsResult{Rows: []model.FileVersion{{Size: 1}}}}
	out := m.applyFindVersionsMsg(in)
	if out.findRows != nil {
		t.Errorf("stale msg applied: rows=%+v", out.findRows)
	}
}

// Cursor moves are paused while a find is loading, since the row set is about
// to be replaced.
func TestFindVersionsCursorPausedWhileLoading(t *testing.T) {
	m := Model{view: findVersionsView, findLoading: true, findCursor: 0}
	m.keys = defaultKeys()
	m.ctx = context.Background()
	next, _ := m.handleFindVersionsKey(press("j"))
	if next.(Model).findCursor != 0 {
		t.Errorf("cursor moved while loading: cursor = %d, want 0", next.(Model).findCursor)
	}
}
