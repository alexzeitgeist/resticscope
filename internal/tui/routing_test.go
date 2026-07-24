package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"unsafe"

	tea "charm.land/bubbletea/v2"
)

func envHas(env []string, kv string) bool {
	return slices.Contains(env, kv)
}

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

func TestBrowseKeysBypassRepoCommandHandler(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a.txt", "a.txt", false, 5))))
	if m.view != browseView {
		t.Fatalf("view = %d, want browseView", m.view)
	}
	if m.browseSnapshotPtr() == nil {
		t.Fatal("browseSnapshotPtr() = nil, want the browsed snapshot to scope the shell to")
	}

	if _, cmd := m.Update(press("s")); cmd == nil {
		t.Error("s in browse view produced no command, want the browse shell to launch")
	}

	for _, k := range []string{"r", "R"} {
		next, _ := m.Update(press(k))
		nm := next.(Model)
		if len(nm.pending) != 0 {
			t.Errorf("key %q in browse view started a refresh (pending=%v), want none", k, nm.pending)
		}
	}
}

func TestBrowseShellSessionIncludesSnapshot(t *testing.T) {
	m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/a.txt", "a.txt", false, 5))))

	sess, err := m.app.ShellSession(m.browseRepo, m.browseSnapshotPtr())
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	defer func() { _ = sess.Cleanup() }()

	want := "RESTICSCOPE_SNAPSHOT_ID=" + m.browseSnapshot
	if !envHas(sess.Env, want) {
		t.Errorf("browse shell env missing %q\n env = %v", want, sess.Env)
	}
}

func TestRepoCommandKeysWorkFromDetailView(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("view = %d, want detailView", m.view)
	}
	if m.detailName != "repo-a" {
		t.Fatalf("detailName = %q, want repo-a", m.detailName)
	}

	if _, cmd := m.Update(press("s")); cmd == nil {
		t.Error("s in detail view produced no command, want the detail shell to launch")
	}

	if next, _ := m.Update(press("r")); !next.(Model).pending["repo-a"] {
		t.Error("r in detail view did not start a refresh for repo-a")
	}

	next, _ := m.Update(press("R"))
	nm := next.(Model)
	for _, r := range m.rows {
		if !nm.pending[r.Name] {
			t.Errorf("R in detail view did not mark %q pending", r.Name)
		}
	}
}

// Compare snapshot IDs because detailSnapshots returns copies, so successive
// selections may have different addresses.
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
		m = update(t, m, press("enter"))
		m = update(t, m, press("j"))
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
		m = update(t, m, press("enter"))
		if m.view != detailView {
			t.Fatalf("view = %d, want detailView", m.view)
		}
		if snap := m.shellSnap(); snap != nil {
			t.Errorf("shellSnap() = %+v, want nil from empty detail view", snap)
		}
	})
}

func TestDetailShellSessionIncludesSnapshot(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	snap := m.shellSnap()
	if snap == nil {
		t.Fatal("shellSnap() = nil, want the highlighted snapshot")
	}

	sess, err := m.app.ShellSession(m.detailName, snap)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	defer func() { _ = sess.Cleanup() }()

	want := "RESTICSCOPE_SNAPSHOT_ID=" + snap.ID
	if !envHas(sess.Env, want) {
		t.Errorf("detail shell env missing %q\n env = %v", want, sess.Env)
	}
}

// Exercise the real s route with a fake shell so correct helper behavior cannot
// mask openShellCmd receiving the wrong snapshot.
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
	m = update(t, m, press("enter"))
	m = update(t, m, press("j"))
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

	// Invoke tea.ExecProcess's wrapper and run its internal command directly so
	// the fake shell records RESTICSCOPE_SNAPSHOT_ID.
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

// extractExecCommand unwraps Bubble Tea's private execMsg command for direct
// execution. tea.ExecProcess otherwise exposes no successful command details.
func extractExecCommand(t *testing.T, msg tea.Msg) tea.ExecCommand {
	t.Helper()
	if msg, ok := msg.(shellExitedMsg); ok {
		t.Fatalf("expected tea.ExecProcess command, got shellExitedMsg: %v", msg.err)
	}
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Struct {
		t.Fatalf("execMsg is %T, want a struct", msg)
	}
	// Copy into an addressable value before accessing the private field.
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
