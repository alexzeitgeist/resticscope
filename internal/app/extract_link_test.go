//go:build linux || darwin

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkNoFollowSemantics pins the two properties publishExtract's
// non-directory branch depends on: (1) linking a symlink node links the
// symlink ITSELF, never its target (darwin's plain link(2) would follow);
// (2) an occupied newname fails EEXIST and never replaces — the property
// os.Rename lacks for non-directory sources, which is the whole reason the
// publish routes through this primitive. The app-level occupant tests cannot
// falsify a stat-then-rename regression (their occupant exists before the
// advisory Lstat, which also refuses), so this is the test that does.
func TestLinkNoFollowSemantics(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	// Dangling target: a primitive that followed the link would fail here.
	mustSetup(t, os.Symlink("dangling-target", node))

	fresh := filepath.Join(dir, "fresh")
	if err := linkNoFollow(node, fresh); err != nil {
		t.Fatalf("linkNoFollow onto a fresh path: %v", err)
	}
	if fi := lstat(t, fresh); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("linked node is not a symlink — the primitive followed the link")
	}
	if tgt, _ := os.Readlink(fresh); tgt != "dangling-target" {
		t.Errorf("linked symlink target = %q, want %q", tgt, "dangling-target")
	}

	occupied := filepath.Join(dir, "occupied")
	mustSetup(t, os.WriteFile(occupied, []byte("occupant"), 0o644))
	if err := linkNoFollow(node, occupied); !errors.Is(err, os.ErrExist) {
		t.Fatalf("linkNoFollow onto an occupant: err = %v, want EEXIST", err)
	}
	if got, err := os.ReadFile(occupied); err != nil || string(got) != "occupant" {
		t.Errorf("occupant must survive untouched, got %q (err %v)", got, err)
	}

	// Contrast pin: rename of a symlink WOULD silently replace the occupant —
	// the verified hazard that rules stat-then-rename out for non-dir leaves.
	if err := os.Rename(node, occupied); err != nil {
		t.Fatalf("os.Rename control: %v", err)
	}
	if fi := lstat(t, occupied); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("control expectation changed: rename no longer replaces — revisit the publish primitive choice")
	}
}
