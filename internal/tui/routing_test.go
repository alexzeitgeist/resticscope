package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	tea "charm.land/bubbletea/v2"
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

	// `s` opens the detail shell.
	if _, cmd := m.Update(press("s")); cmd == nil {
		t.Error("s in detail view produced no command, want the detail shell to launch")
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

// TestShellSnapByView locks the seam that decides what snapshot the `s` key
// scopes to: nil on the list (repo-only shell), the highlighted snapshot in the
// detail view, and nil for an empty detail view (fallback to repo-only shell).
// The detail case asserts identity by ID rather than pointer because
// detailSnapshots() copies into a sorted slice, so successive
// selectedSnapshot() calls can return equivalent snapshots at different
// addresses.
func TestShellSnapByView(t *testing.T) {
	t.Run("list view", func(t *testing.T) {
		m := newTestModel(t, detailApp(t))
		if m.view != listView {
			t.Fatalf("view = %d, want listView", m.view)
		}
		if snap := m.shellSnap(); snap != nil {
			t.Errorf("shellSnap() = %+v, want nil from list view", snap)
		}
	})

	t.Run("detail view with snapshots", func(t *testing.T) {
		m := newTestModel(t, detailApp(t))
		m = update(t, m, press("enter")) // → detail view
		m = update(t, m, press("j"))     // move cursor onto the middle snapshot
		want := m.selectedSnapshot()
		if want == nil {
			t.Fatal("selectedSnapshot() = nil, want a snapshot after enter+j")
		}
		got := m.shellSnap()
		if got == nil {
			t.Fatalf("shellSnap() = nil, want the selected snapshot")
		}
		if got.ID != want.ID || got.ShortID != want.ShortID {
			t.Errorf("shellSnap() = {ID:%q ShortID:%q}, want {ID:%q ShortID:%q}",
				got.ID, got.ShortID, want.ID, want.ShortID)
		}
	})

	t.Run("detail view with empty snapshot list", func(t *testing.T) {
		a := detailApp(t)
		cache := a.Cache.(stubCache)
		st := cache.states["repo-a"]
		st.Snapshots = nil
		st.SnapshotCount = 0
		cache.states["repo-a"] = st

		m := newTestModel(t, a)
		m = update(t, m, press("enter")) // → detail view
		if m.view != detailView {
			t.Fatalf("view = %d, want detailView", m.view)
		}
		if snap := m.shellSnap(); snap != nil {
			t.Errorf("shellSnap() = %+v, want nil from empty detail view", snap)
		}
	})
}

// TestDetailShellSessionIncludesSnapshot mirrors TestBrowseShellSessionIncludesSnapshot
// for the detail view: it proves the App/ShellSession boundary scopes to the
// highlighted snapshot when shellSnap() resolves one.
func TestDetailShellSessionIncludesSnapshot(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter")) // → detail view
	snap := m.shellSnap()
	if snap == nil {
		t.Fatal("shellSnap() = nil, want the highlighted snapshot")
	}

	sess, err := m.app.ShellSession(m.detailName, snap)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	defer sess.Cleanup() // no-op in env mode (detailApp sets ShellPasswordMode="env")

	want := "RESTICSCOPE_SNAPSHOT_ID=" + snap.ID
	if !envHas(sess.Env, want) {
		t.Errorf("detail shell env missing %q\n env = %v", want, sess.Env)
	}
}

// TestDetailShellKeyRoutePassesSnapshot drives the real `s` route end-to-end —
// not just shellSnap() or ShellSession() in isolation — by running the returned
// command through a tiny fake shell that records $RESTICSCOPE_SNAPSHOT_ID. This
// closes the gap where the seam and the boundary could both be correct while
// the route layer accidentally still calls openShellCmd(nil).
func TestDetailShellKeyRoutePassesSnapshot(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "snapshot-id")
	shellPath := filepath.Join(dir, "fakeshell")
	script := "#!/bin/sh\nprintf '%s' \"$RESTICSCOPE_SNAPSHOT_ID\" > " + outPath + "\nexit 0\n"
	if err := os.WriteFile(shellPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}

	a := detailApp(t)
	a.Cfg.Global.Shell = shellPath

	m := newTestModel(t, a)
	m = update(t, m, press("enter")) // → detail view
	m = update(t, m, press("j"))     // move cursor to a known non-default snapshot
	want := m.selectedSnapshot()
	if want == nil {
		t.Fatal("selectedSnapshot() = nil, want a snapshot after enter+j")
	}

	next, cmd := m.Update(press("s"))
	if _, ok := next.(Model); !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	if cmd == nil {
		t.Fatal("s in detail view produced no command, want the shell to launch")
	}

	// The returned command is tea.ExecProcess's wrapper: invoking it yields the
	// internal execMsg holding an ExecCommand. The real tea.Program runs that
	// command on the main thread; here we extract it and Run() it directly so
	// the fake shell actually executes and records $RESTICSCOPE_SNAPSHOT_ID.
	ec := extractExecCommand(t, cmd())
	if err := ec.Run(); err != nil {
		t.Fatalf("ExecCommand.Run: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read fake shell output: %v", err)
	}
	if string(got) != want.ID {
		t.Errorf("RESTICSCOPE_SNAPSHOT_ID = %q, want %q", string(got), want.ID)
	}
}

// extractExecCommand unwraps the unexported `cmd` field from a bubbletea
// execMsg via reflection so a test can Run() the wrapped command directly,
// without driving a real tea.Program. This is the only way to observe what
// openShellCmd actually exec'd, because tea.ExecProcess returns an opaque
// success-path command and execMsg's fields are package-private.
func extractExecCommand(t *testing.T, msg tea.Msg) tea.ExecCommand {
	t.Helper()
	if msg, ok := msg.(shellExitedMsg); ok {
		t.Fatalf("expected tea.ExecProcess command, got shellExitedMsg: %v", msg.err)
	}
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Struct {
		t.Fatalf("execMsg is %T, want a struct", msg)
	}
	// reflect.ValueOf returns a non-addressable Value, so copy into a fresh
	// addressable cell before grabbing the unexported field's address.
	addr := reflect.New(v.Type()).Elem()
	addr.Set(v)
	f := addr.FieldByName("cmd")
	if !f.IsValid() {
		t.Fatalf("msg %T has no field 'cmd'", msg)
	}
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	ec, ok := f.Interface().(tea.ExecCommand)
	if !ok {
		t.Fatalf("execMsg.cmd is %T, want tea.ExecCommand", f.Interface())
	}
	return ec
}
