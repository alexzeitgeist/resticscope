//go:build linux || darwin

package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"resticscope/internal/resticx"

	"golang.org/x/sys/unix"
)

// --- direct normalizer unit test: everything intrinsic is PRESERVED ---

func TestNormalizeExtractTreeMetadata(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	file := filepath.Join(root, "file")
	mustSetup(t, os.WriteFile(file, []byte("hello"), 0o666))
	mustSetup(t, os.Chmod(file, 0o666|os.ModeSetuid)) // permissive + suid → must be PRESERVED
	sub := filepath.Join(root, "sub")
	mustSetup(t, os.Mkdir(sub, 0o777))
	mustSetup(t, os.Chmod(sub, 0o777|os.ModeSticky)) // permissive + sticky → PRESERVED
	nested := filepath.Join(sub, "nested")
	mustSetup(t, os.WriteFile(nested, []byte("x"), 0o644))
	link := filepath.Join(root, "link")
	mustSetup(t, os.Symlink("sub/nested", link)) // relative, non-escaping → safe

	xattrSet := trySetXattr(t, file)

	// Backdate mtimes so "preserved" is observable against a concrete instant.
	for _, p := range []string{file, nested, sub} {
		mustSetup(t, os.Chtimes(p, old, old))
	}
	// Backdate the symlink's OWN mtime too (Lutimes is no-follow, unlike Chtimes).
	tvOld := unix.NsecToTimeval(old.UnixNano())
	mustSetup(t, unix.Lutimes(link, []unix.Timeval{tvOld, tvOld}))

	counts, err := normalizeExtractTreeMetadata(context.Background(), root, unsafeSymlinkKeep)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if counts.Files != 2 || counts.Dirs != 1 || counts.UnsafeSymlinks != 0 || counts.Other != 0 {
		t.Errorf("counts = %+v, want Files=2 Dirs=1 UnsafeSymlinks=0 Other=0 (root excluded)", counts)
	}

	fi := lstat(t, file)
	if fi.Mode().Perm() != 0o666 || fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("file mode = %v, want 0666 with setuid preserved", fi.Mode())
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("file mtime = %v, want preserved %v", fi.ModTime(), old)
	}

	di := lstat(t, sub)
	if di.Mode().Perm() != 0o777 || di.Mode()&os.ModeSticky == 0 {
		t.Errorf("dir mode = %v, want 0777 with sticky preserved", di.Mode())
	}
	if !di.ModTime().Equal(old) {
		t.Errorf("dir mtime = %v, want preserved %v", di.ModTime(), old)
	}

	if ni := lstat(t, nested); ni.Mode().Perm() != 0o644 {
		t.Errorf("nested file mode = %v, want 0644 preserved", ni.Mode())
	}

	if li := lstat(t, link); li.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was not preserved as a symlink (must not be followed)")
	}
	if li := lstat(t, link); !li.ModTime().Equal(old) {
		t.Errorf("symlink mtime = %v, want preserved %v (never touched)", li.ModTime(), old)
	}
	if tgt, _ := os.Readlink(link); tgt != "sub/nested" {
		t.Errorf("symlink target = %q, want unchanged %q", tgt, "sub/nested")
	}

	if xattrSet && !hasXattr(t, file, "user.resticscope_test") {
		t.Error("xattr was not preserved on the regular file")
	}
}

// A directory restored without owner r/w/x (e.g. mode 0000) must still be walked:
// the pass forces it owner-rwx before reading its children, then RESTORES the
// original mode. Without the pre-read chmod, ReadDir would fail with EACCES.
func TestNormalizeExtractTreeMetadataRestrictiveDir(t *testing.T) {
	root := t.TempDir()

	child := filepath.Join(root, "locked")
	mustSetup(t, os.Mkdir(child, 0o700))
	mustSetup(t, os.WriteFile(filepath.Join(child, "inner"), []byte("x"), 0o600))
	mustSetup(t, os.Chmod(child, 0o000)) // no owner r/w/x
	// Load-bearing now: the normalizer RESTORES 0000, so t.TempDir's RemoveAll
	// can't enter the dir it leaves behind without this chmod-up.
	t.Cleanup(func() { _ = os.Chmod(child, 0o700) })

	counts, err := normalizeExtractTreeMetadata(context.Background(), root, unsafeSymlinkKeep)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if di := lstat(t, child); di.Mode().Perm() != 0o000 {
		t.Errorf("restrictive dir mode = %v, want restored to 0000", di.Mode())
	}
	// A 0000 dir is not searchable, so chmod up just to inspect the inner file.
	mustSetup(t, os.Chmod(child, 0o700))
	if fi := lstat(t, filepath.Join(child, "inner")); fi.Mode().Perm() != 0o600 {
		t.Errorf("inner file mode = %v, want 0600 preserved", fi.Mode())
	}
	if counts.Files != 1 || counts.Dirs != 1 {
		t.Errorf("counts = %+v, want Files=1 Dirs=1", counts)
	}
}

// --- pure classifier: the empty/NUL/absolute/escaping/safe rule ---

// TestClassifySymlinkTarget exercises classifySymlinkTarget directly: it needs no
// on-disk link, which is the only way to cover the empty/NUL guards (os.Symlink
// cannot create those targets).
func TestClassifySymlinkTarget(t *testing.T) {
	const root = "/stage/root"
	const parent = "/stage/root/sub"
	cases := []struct {
		name   string
		target string
		unsafe bool
	}{
		{"empty", "", true},
		{"nul", "tar\x00get", true},
		{"absolute", "/etc/passwd", true},
		{"escaping", "../../etc/passwd", true},
		{"escaping to filesystem root", "../../../", true},
		{"safe nested", "deeper/file", false},
		{"safe dot", ".", false},
		{"safe up but within root", "../within", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifySymlinkTarget(root, parent, c.target); got != c.unsafe {
				t.Errorf("classifySymlinkTarget(%q) = %v, want unsafe=%v", c.target, got, c.unsafe)
			}
		})
	}
}

// --- on-disk unsafe-symlink policy matrix ---

// TestNormalizeExtractTreeUnsafeSymlinks drives every policy against absolute,
// escaping, and safe links, plus a restricted (0500) parent and a fifo, asserting
// the normalizer never aborts and produces the right on-disk outcome and counts.
func TestNormalizeExtractTreeUnsafeSymlinks(t *testing.T) {
	kinds := []struct {
		name   string
		target string
		unsafe bool
	}{
		{"absolute", "/etc/passwd", true},
		{"escaping", "../../../etc", true},
		{"safe", "sibling", false},
	}
	policies := []unsafeSymlinkPolicy{unsafeSymlinkKeep, unsafeSymlinkSkip, unsafeSymlinkPlaceholder}

	for _, pol := range policies {
		for _, k := range kinds {
			t.Run(string(pol)+"/"+k.name, func(t *testing.T) {
				root := t.TempDir()
				link := filepath.Join(root, "link")
				mustSetup(t, os.Symlink(k.target, link))

				counts, err := normalizeExtractTreeMetadata(context.Background(), root, pol)
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}
				wantUnsafe := 0
				if k.unsafe {
					wantUnsafe = 1
				}
				if counts.UnsafeSymlinks != wantUnsafe {
					t.Errorf("UnsafeSymlinks = %d, want %d", counts.UnsafeSymlinks, wantUnsafe)
				}
				assertSymlinkOutcome(t, link, k.target, k.unsafe, pol)
			})
		}
	}

	// A 0500 parent holding an unsafe symlink: skip/placeholder mutate a CHILD, so
	// the pass must temp-chmod the parent writable (0o700, not 0o500) and restore
	// its 0500 mode afterward. No EACCES.
	for _, pol := range []unsafeSymlinkPolicy{unsafeSymlinkSkip, unsafeSymlinkPlaceholder} {
		t.Run("restricted-parent/"+string(pol), func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "ro")
			mustSetup(t, os.Mkdir(parent, 0o700))
			link := filepath.Join(parent, "link")
			mustSetup(t, os.Symlink("/etc/passwd", link)) // absolute → unsafe
			mustSetup(t, os.Chmod(parent, 0o500))         // r-x: traversable, not writable
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

			counts, err := normalizeExtractTreeMetadata(context.Background(), root, pol)
			if err != nil {
				t.Fatalf("normalize under a 0500 parent: %v", err)
			}
			if counts.UnsafeSymlinks != 1 {
				t.Errorf("UnsafeSymlinks = %d, want 1", counts.UnsafeSymlinks)
			}
			if pol == unsafeSymlinkSkip {
				if _, err := os.Lstat(link); !os.IsNotExist(err) {
					t.Errorf("skip under 0500 parent did not remove the link: %v", err)
				}
			} else if fi := lstat(t, link); !fi.Mode().IsRegular() {
				t.Errorf("placeholder under 0500 parent is not a regular file: %v", fi.Mode())
			}
			if di := lstat(t, parent); di.Mode().Perm() != 0o500 {
				t.Errorf("parent mode = %v, want restored to 0500", di.Mode())
			}
		})
	}

	// A fifo is a special node: counted under Other, left in place, never an abort.
	t.Run("fifo special node", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		counts, err := normalizeExtractTreeMetadata(context.Background(), root, unsafeSymlinkKeep)
		if err != nil {
			t.Fatalf("normalize with a fifo: %v", err)
		}
		if counts.Other != 1 {
			t.Errorf("Other = %d, want 1", counts.Other)
		}
		if fi := lstat(t, filepath.Join(root, "pipe")); fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("fifo not preserved: mode = %v", fi.Mode())
		}
	})
}

// assertSymlinkOutcome checks the on-disk state of a link after a policy ran. A
// safe link, and any link under keep, is left verbatim; skip removes it;
// placeholder replaces it with a 0600 regular file recording the target.
func assertSymlinkOutcome(t *testing.T, link, target string, unsafe bool, pol unsafeSymlinkPolicy) {
	t.Helper()
	if !unsafe || pol == unsafeSymlinkKeep {
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("link missing, want kept: %v", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("kept link is no longer a symlink: %v", fi.Mode())
		}
		if got, _ := os.Readlink(link); got != target {
			t.Errorf("kept link target = %q, want %q", got, target)
		}
		return
	}
	switch pol {
	case unsafeSymlinkSkip:
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("skip should remove the link; lstat err = %v", err)
		}
	case unsafeSymlinkPlaceholder:
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("placeholder file missing: %v", err)
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Errorf("placeholder mode = %v, want a 0600 regular file", fi.Mode())
		}
		data, err := os.ReadFile(link)
		if err != nil {
			t.Fatalf("read placeholder: %v", err)
		}
		if string(data) != target+"\n" {
			t.Errorf("placeholder contents = %q, want %q", string(data), target+"\n")
		}
	}
}

// --- live tree extract (real run drives the normalizer before rename) ---

func TestExtractDirectoryTreeRealRun(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	setup := func(target string) error {
		conf := filepath.Join(target, "nginx.conf")
		if err := os.WriteFile(conf, []byte("server {}"), 0o666); err != nil {
			return err
		}
		if err := os.Chmod(conf, 0o666|os.ModeSetgid); err != nil {
			return err
		}
		return os.Chtimes(conf, old, old)
	}
	a, fc := newExtractApp(fakeRestic{
		extractCap:        cap,
		extractTreeSetup:  setup,
		extractTreeEvents: []resticx.ExtractTreeEvent{{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 9}},
	}, root)
	req := treeReq()
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	cap.mu.Lock()
	if cap.treeParams.Target != staging || cap.treeParams.Source != req.Source {
		t.Errorf("tree params = %+v, want Target=%q Source=%q", cap.treeParams, staging, req.Source)
	}
	cap.mu.Unlock()

	if result.FinalDir != final {
		t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
	}
	// The normalizer ran before the rename and PRESERVED the restored metadata: the
	// file is still 0666 with its setgid bit and original mtime, under the final dir.
	fi := lstat(t, filepath.Join(final, "nginx.conf"))
	if fi.Mode().Perm() != 0o666 || fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("file mode = %v, want 0666 with setgid preserved", fi.Mode())
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("file mtime = %v, want preserved %v", fi.ModTime(), old)
	}
	if result.Files != 1 || result.Dirs != 0 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 0} from the normalizer walk", result.Files, result.Dirs)
	}
	if result.UnsafeSymlinks != 0 || result.Other != 0 {
		t.Errorf("result = {UnsafeSymlinks:%d Other:%d}, want {0 0}", result.UnsafeSymlinks, result.Other)
	}
	if _, serr := os.Stat(staging); serr == nil {
		t.Error("staging dir still present after a successful rename")
	}
	if len(fc.saved) != 0 {
		t.Errorf("tree extract wrote %d cache entries, want 0", len(fc.saved))
	}
}

// TestExtractDirectoryTreeRootModeIsPrivate pins the privacy invariant that now
// rests on two external facts: resticscope creates staging 0700, and restic's
// subpath restore never applies the source dir's own mode to --target. The
// rewritten normalizer no longer force-chmods the root, so the published output
// root must still be 0700 while a restored CHILD directory keeps its (wider)
// restic mode. A future change to root handling that widened the container would
// trip this.
func TestExtractDirectoryTreeRootModeIsPrivate(t *testing.T) {
	root := t.TempDir()
	setup := func(target string) error {
		sub := filepath.Join(target, "conf.d")
		if err := os.Mkdir(sub, 0o750); err != nil {
			return err
		}
		if err := os.Chmod(sub, 0o750|os.ModeSetgid); err != nil { // wider than 0700 + setgid → preserved
			return err
		}
		return os.WriteFile(filepath.Join(sub, "site"), []byte("x"), 0o640)
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	req := treeReq()
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// The per-op output root is resticscope's private container, not user data.
	if di := lstat(t, final); di.Mode().Perm() != 0o700 {
		t.Errorf("final output root mode = %v, want 0700 (private container)", di.Mode())
	}
	// The restored child directory keeps its restic mode, including setgid.
	if cd := lstat(t, filepath.Join(final, "conf.d")); cd.Mode().Perm() != 0o750 || cd.Mode()&os.ModeSetgid == 0 {
		t.Errorf("child dir mode = %v, want 0750 with setgid preserved", cd.Mode())
	}
	if result.Dirs != 1 {
		t.Errorf("result.Dirs = %d, want 1", result.Dirs)
	}
}

// TestExtractDirectoryTreeSpecialFilePublishes proves the rewritten normalizer no
// longer aborts on special files or unsafe symlinks: a fifo and an absolute
// (unsafe) symlink are counted, left in place, and the staging dir is published.
// "Leaves staging on an interruption" stays covered by the cancel/timeout tests;
// the only remaining normalizer hard-failure is a genuine lstat/readdir IO error.
func TestExtractDirectoryTreeSpecialFilePublishes(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, ".probe")
	if err := syscall.Mkfifo(probe, 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	mustSetup(t, os.Remove(probe))

	setup := func(target string) error {
		if err := syscall.Mkfifo(filepath.Join(target, "pipe"), 0o644); err != nil {
			return err
		}
		return os.Symlink("/etc/passwd", filepath.Join(target, "abslink")) // unsafe, kept by default
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	req := treeReq()
	req.Source = "/etc/special-dir"
	req.SourceName = "special-dir"
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.FinalDir != final {
		t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
	}
	if result.Other != 1 {
		t.Errorf("Other = %d, want 1 (the fifo)", result.Other)
	}
	if result.UnsafeSymlinks != 1 {
		t.Errorf("UnsafeSymlinks = %d, want 1 (the absolute link, kept)", result.UnsafeSymlinks)
	}
	if fi := lstat(t, filepath.Join(final, "pipe")); fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo not published: mode = %v", fi.Mode())
	}
	if fi := lstat(t, filepath.Join(final, "abslink")); fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("unsafe symlink not published: mode = %v", fi.Mode())
	}
	if _, serr := os.Stat(staging); serr == nil {
		t.Error("staging dir still present after a successful rename")
	}
}

func TestExtractTreeLoggingIsPathFree(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	setup := func(target string) error {
		if err := os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600); err != nil {
			return err
		}
		return os.Symlink("/etc/shadow", filepath.Join(target, "leak-link")) // unsafe → counted
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))
	req := treeReq()
	req.Source = "/etc/topsecret-tree"
	req.SourceName = "topsecret-tree"

	if _, err := a.Extract(context.Background(), req, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	logged := buf.String()
	for _, leak := range []string{"topsecret-tree", root} {
		if strings.Contains(logged, leak) {
			t.Errorf("tree extract log leaked %q:\n%s", leak, logged)
		}
	}
	if !strings.Contains(logged, "extract.tree") {
		t.Errorf("expected an extract.tree finish line, got:\n%s", logged)
	}
	if !strings.Contains(logged, "unsafe_symlinks=1") {
		t.Errorf("expected unsafe_symlinks=1 in the finish line, got:\n%s", logged)
	}
}

// --- helpers ---

func mustSetup(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func lstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return fi
}

// trySetXattr sets a user xattr, returning false (so the caller skips the
// xattr-preservation assertion) when the filesystem does not support it.
func trySetXattr(t *testing.T, p string) bool {
	t.Helper()
	if err := unix.Lsetxattr(p, "user.resticscope_test", []byte("1"), 0); err != nil {
		t.Logf("user xattr unsupported on this filesystem (%v); skipping xattr assertion", err)
		return false
	}
	return true
}

func hasXattr(t *testing.T, p, name string) bool {
	t.Helper()
	sz, err := unix.Llistxattr(p, nil)
	if err != nil || sz == 0 {
		return false
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return false
	}
	for _, b := range bytes.Split(buf[:sz], []byte{0}) {
		if string(b) == name {
			return true
		}
	}
	return false
}
