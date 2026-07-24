//go:build linux || darwin

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkNoFollowSemantics pins non-following symlink links and EEXIST refusal.
// App-level occupant tests cannot detect stat-then-rename because their early
// Lstat also refuses, so the rename control below demonstrates that hazard.
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

	// Renaming the symlink demonstrates the replacement hazard this avoids.
	if err := os.Rename(node, occupied); err != nil {
		t.Fatalf("os.Rename control: %v", err)
	}
	if fi := lstat(t, occupied); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("control expectation changed: rename no longer replaces — revisit the publish primitive choice")
	}
}
