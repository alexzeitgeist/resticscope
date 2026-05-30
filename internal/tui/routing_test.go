package tui

import (
	"testing"
)

// envHas reports whether env contains an exact "KEY=value" entry.
func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestRepoCommandKeysAreIgnoredByHelpOverlay locks that the help overlay is modal:
// while it is showing, the repo command keys (shell, refresh, refresh-all) do
// nothing — handleKey short-circuits in helpView before the repo command handler
// is reached — and only Back closes the overlay.
func TestRepoCommandKeysAreIgnoredByHelpOverlay(t *testing.T) {
	m := newTestModel(t, testApp(nil))
	m = update(t, m, press("?"))
	if m.view != helpView {
		t.Fatalf("view = %d, want helpView", m.view)
	}

	for _, k := range []string{"s", "r", "R"} {
		next, cmd := m.Update(press(k))
		nm := next.(Model)
		if cmd != nil {
			t.Errorf("key %q in help overlay produced a command, want none", k)
		}
		if len(nm.pending) != 0 {
			t.Errorf("key %q in help overlay started a refresh (pending=%v), want none", k, nm.pending)
		}
		if nm.view != helpView {
			t.Errorf("key %q left the help overlay, view = %d, want helpView", k, nm.view)
		}
	}

	nm := update(t, m, press("esc"))
	if nm.view != listView {
		t.Fatalf("after Back, view = %d, want listView", nm.view)
	}
}

// TestBrowseKeysBypassRepoCommandHandler locks that browse view is dispatched
// before the repo command keys, so the shared refresh/shell handlers cannot steal
// browse keys.
func TestBrowseKeysBypassRepoCommandHandler(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a.txt", "a.txt", false, 5))))
	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if m.browseSnapshotPtr() == nil {
		t.Fatal("browseSnapshotPtr() = nil, want the browsed snapshot to scope the shell to")
	}

	// `s` in browse view launches the browse shell (scoped to the snapshot), not a
	// no-op: a non-nil command proves the browse handler ran.
	if _, cmd := m.Update(press("s")); cmd == nil {
		t.Error("s in browse view produced no command, want the browse shell to launch")
	}

	// `r`/`R` are repo command keys; in browse view they reach handleBrowseKey,
	// which ignores them. If the dispatch order regressed they would start a
	// refresh, observable as a pending entry.
	for _, k := range []string{"r", "R"} {
		next, _ := m.Update(press(k))
		nm := next.(Model)
		if len(nm.pending) != 0 {
			t.Errorf("key %q in browse view started a refresh (pending=%v), want none", k, nm.pending)
		}
	}
}

// TestBrowseShellSessionIncludesSnapshot proves the browse shell scopes to the
// browsed snapshot, observed at the exported App.ShellSession boundary (the exec
// command itself is opaque).
func TestBrowseShellSessionIncludesSnapshot(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a.txt", "a.txt", false, 5))))

	sess, err := m.app.ShellSession(m.browseRepo, m.browseSnapshotPtr())
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	defer sess.Cleanup() // no-op in env mode (browseApp sets ShellPasswordMode="env")

	want := "RESTICSCOPE_SNAPSHOT_ID=" + m.browseSnapshot
	if !envHas(sess.Env, want) {
		t.Errorf("browse shell env missing %q\n env = %v", want, sess.Env)
	}
}

// TestRepoCommandKeysWorkFromDetailView locks that shell, refresh, and refresh-all
// all act on the detail view's pinned repo (and every repo, for refresh-all).
func TestRepoCommandKeysWorkFromDetailView(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("view = %d, want detailView", m.view)
	}
	if m.detailName != "repo-a" {
		t.Fatalf("detailName = %q, want repo-a", m.detailName)
	}

	// `s` opens a repo shell.
	if _, cmd := m.Update(press("s")); cmd == nil {
		t.Error("s in detail view produced no command, want the repo shell to launch")
	}

	// `r` refreshes the detail repo.
	if next, _ := m.Update(press("r")); !next.(Model).pending["repo-a"] {
		t.Error("r in detail view did not start a refresh for repo-a")
	}

	// `R` refreshes every repo, from a fresh detail model.
	next, _ := m.Update(press("R"))
	nm := next.(Model)
	for _, r := range m.rows {
		if !nm.pending[r.Name] {
			t.Errorf("R in detail view did not mark %q pending", r.Name)
		}
	}
}
