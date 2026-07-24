//go:build linux || darwin

package app

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/resticx"

	"golang.org/x/sys/unix"
)

func TestNormalizeExtractTreeMetadata(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	file := filepath.Join(root, "file")
	mustSetup(t, os.WriteFile(file, []byte("hello"), 0o666))
	mustSetup(t, os.Chmod(file, 0o666|os.ModeSetuid)) // permissive + suid -> must be PRESERVED
	sub := filepath.Join(root, "sub")
	mustSetup(t, os.Mkdir(sub, 0o777))
	mustSetup(t, os.Chmod(sub, 0o777|os.ModeSticky)) // permissive + sticky -> PRESERVED
	nested := filepath.Join(sub, "nested")
	mustSetup(t, os.WriteFile(nested, []byte("x"), 0o644))
	link := filepath.Join(root, "link")
	mustSetup(t, os.Symlink("sub/nested", link)) // relative, non-escaping -> safe

	xattrSet := trySetXattr(t, file)

	// Backdate mtimes so "preserved" is observable against a concrete instant.
	for _, p := range []string{file, nested, sub} {
		mustSetup(t, os.Chtimes(p, old, old))
	}
	// Backdate the symlink's OWN mtime too (Lutimes is no-follow, unlike Chtimes).
	tvOld := unix.NsecToTimeval(old.UnixNano())
	mustSetup(t, unix.Lutimes(link, []unix.Timeval{tvOld, tvOld}))

	counts, err := normalizeExtractTreeMetadata(t.Context(), root, unsafeSymlinkKeep)
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

// Restrictive directories require temporary owner rwx for traversal, followed
// by restoration of the original mode.
func TestNormalizeExtractTreeMetadataRestrictiveDir(t *testing.T) {
	root := t.TempDir()

	child := filepath.Join(root, "locked")
	mustSetup(t, os.Mkdir(child, 0o700))
	mustSetup(t, os.WriteFile(filepath.Join(child, "inner"), []byte("x"), 0o600))
	mustSetup(t, os.Chmod(child, 0o000)) // no owner r/w/x
	// TempDir cleanup needs access after the normalizer restores mode 0000.
	t.Cleanup(func() { _ = os.Chmod(child, 0o700) })

	counts, err := normalizeExtractTreeMetadata(t.Context(), root, unsafeSymlinkKeep)
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

// TestClassifySymlinkTarget calls the pure classifier because os.Symlink cannot
// create empty or NUL-bearing targets.
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

// TestNormalizeExtractTreeUnsafeSymlinks covers every policy, restricted parent
// mutation, and special-node preservation.
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

				counts, err := normalizeExtractTreeMetadata(t.Context(), root, pol)
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

	// Link mutation requires temporary parent write permission, then mode restoration.
	for _, pol := range []unsafeSymlinkPolicy{unsafeSymlinkSkip, unsafeSymlinkPlaceholder} {
		t.Run("restricted-parent/"+string(pol), func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "ro")
			mustSetup(t, os.Mkdir(parent, 0o700))
			link := filepath.Join(parent, "link")
			mustSetup(t, os.Symlink("/etc/passwd", link)) // absolute -> unsafe
			mustSetup(t, os.Chmod(parent, 0o500))         // r-x: traversable, not writable
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

			counts, err := normalizeExtractTreeMetadata(t.Context(), root, pol)
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

	t.Run("fifo special node", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		counts, err := normalizeExtractTreeMetadata(t.Context(), root, unsafeSymlinkKeep)
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

// assertSymlinkOutcome checks the persisted result of each link policy.
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

func TestExtractDirectoryTreeRealRun(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	// Restic reconstructs the selected leaf and its contents under staging.
	setup := dirStagingSetup("nginx", func(node string) error {
		conf := filepath.Join(node, "nginx.conf")
		if err := os.WriteFile(conf, []byte("server {}"), 0o666); err != nil {
			return err
		}
		if err := os.Chmod(conf, 0o666|os.ModeSetgid); err != nil {
			return err
		}
		return os.Chtimes(conf, old, old)
	})
	a, fc := newExtractApp(fakeRestic{
		extractCap:        cap,
		extractTreeSetup:  setup,
		extractTreeEvents: []resticx.ExtractTreeEvent{{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 9}},
	}, root)
	req := treeReq()
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(t.Context(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Rebase plus include makes restic create the leaf and apply its metadata.
	cap.mu.Lock()
	tp := cap.treeParams
	cap.mu.Unlock()
	wantInc := []string{"/" + path.Base(req.Source)}
	if tp.Target != staging || tp.Source != path.Dir(req.Source) || !slices.Equal(tp.IncludePaths, wantInc) {
		t.Errorf("tree params = %+v, want Target=%q Source=%q IncludePaths=%q", tp, staging, path.Dir(req.Source), wantInc)
	}

	if result.FinalDir != final {
		t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
	}
	if result.FinalPath != final {
		t.Errorf("FinalPath = %q, want %q", result.FinalPath, final)
	}
	// Normalization precedes publish and preserves restored metadata.
	fi := lstat(t, filepath.Join(final, "nginx.conf"))
	if fi.Mode().Perm() != 0o666 || fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("file mode = %v, want 0666 with setgid preserved", fi.Mode())
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("file mtime = %v, want preserved %v", fi.ModTime(), old)
	}
	if result.Files != 1 || result.Dirs != 1 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 1} from the normalizer walk", result.Files, result.Dirs)
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

// TestExtractDirectoryLeafMetadataPreserved guards against restoring only a
// directory's contents into a pre-made 0700 root. Restic must create the leaf
// with snapshot metadata while synthesized ancestors remain private.
func TestExtractDirectoryLeafMetadataPreserved(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	// Model a reconstructed leaf with snapshot mode and mtime.
	setup := func(target string) error {
		node := filepath.Join(target, "nginx")
		if err := os.Mkdir(node, 0o750); err != nil {
			return err
		}
		if err := os.Chmod(node, 0o750|os.ModeSetgid); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(node, "site"), []byte("x"), 0o640); err != nil {
			return err
		}
		return os.Chtimes(node, old, old) // last, so writing the child doesn't bump it
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	req := treeReq()
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(t.Context(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	di := lstat(t, final)
	if di.Mode().Perm() != 0o750 || di.Mode()&os.ModeSetgid == 0 {
		t.Errorf("leaf dir mode = %v, want 0750 with setgid preserved (not 0700)", di.Mode())
	}
	if !di.ModTime().Equal(old) {
		t.Errorf("leaf dir mtime = %v, want preserved %v (not extract time)", di.ModTime(), old)
	}
	if pd := lstat(t, filepath.Dir(final)); pd.Mode().Perm() != 0o700 {
		t.Errorf("scaffolding ancestor mode = %v, want 0700 (private)", pd.Mode())
	}
	if result.Files != 1 || result.Dirs != 1 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 1}", result.Files, result.Dirs)
	}
}

// TestExtractDirectoryTreeSpecialFilePublishes verifies that special files and
// unsafe links are counted and published rather than treated as fatal.
func TestExtractDirectoryTreeSpecialFilePublishes(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, ".probe")
	if err := syscall.Mkfifo(probe, 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	mustSetup(t, os.Remove(probe))

	setup := dirStagingSetup("special-dir", func(node string) error {
		if err := syscall.Mkfifo(filepath.Join(node, "pipe"), 0o644); err != nil {
			return err
		}
		return os.Symlink("/etc/passwd", filepath.Join(node, "abslink")) // unsafe, kept by default
	})
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	req := treeReq()
	req.Source = "/etc/special-dir"
	req.SourceName = "special-dir"
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(t.Context(), req, nil)
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
	setup := dirStagingSetup("topsecret-tree", func(node string) error {
		if err := os.WriteFile(filepath.Join(node, "f"), []byte("x"), 0o600); err != nil {
			return err
		}
		return os.Symlink("/etc/shadow", filepath.Join(node, "leak-link")) // unsafe -> counted
	})
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))
	req := treeReq()
	req.Source = "/etc/topsecret-tree"
	req.SourceName = "topsecret-tree"

	if _, err := a.Extract(t.Context(), req, nil); err != nil {
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

// TestNormalizeExtractPlaceholderConfinedToStaging verifies that replacement
// through os.Root cannot modify an absolute external target.
func TestNormalizeExtractPlaceholderConfinedToStaging(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "staging")
	mustSetup(t, os.Mkdir(root, 0o700))

	victim := filepath.Join(base, "victim.txt")
	mustSetup(t, os.WriteFile(victim, []byte("LIVE-SECRET\n"), 0o600))

	link := filepath.Join(root, "link")
	mustSetup(t, os.Symlink(victim, link)) // absolute -> unsafe

	counts, err := normalizeExtractTreeMetadata(t.Context(), root, unsafeSymlinkPlaceholder)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if counts.UnsafeSymlinks != 1 {
		t.Errorf("UnsafeSymlinks = %d, want 1", counts.UnsafeSymlinks)
	}
	if fi := lstat(t, link); !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("placeholder mode = %v, want a 0600 regular file", fi.Mode())
	}
	if data, _ := os.ReadFile(link); string(data) != victim+"\n" {
		t.Errorf("placeholder contents = %q, want %q", string(data), victim+"\n")
	}
	// A followed link would have replaced the external content.
	if data, _ := os.ReadFile(victim); string(data) != "LIVE-SECRET\n" {
		t.Errorf("external victim content = %q, want it unchanged", string(data))
	}
	if fi := lstat(t, victim); fi.Mode().Perm() != 0o600 {
		t.Errorf("external victim mode = %v, want unchanged 0600", fi.Mode())
	}
}

// TestChownStagingForCleanup uses chown-to-self to exercise permission widening
// and os.Root confinement without root.
func TestChownStagingForCleanup(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()

	t.Run("widens dirs so the user can remove the tree", func(t *testing.T) {
		base := t.TempDir()
		staging := filepath.Join(base, "staging")
		mustSetup(t, os.Mkdir(staging, 0o700))
		locked := filepath.Join(staging, "locked")
		mustSetup(t, os.Mkdir(locked, 0o700))
		mustSetup(t, os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600))
		mustSetup(t, os.Chmod(locked, 0o500)) // r-x: traversable, not writable for unlink

		chownStagingForCleanup(staging, uid, gid)

		if di := lstat(t, locked); di.Mode().Perm()&0o700 != 0o700 {
			t.Errorf("locked dir mode = %v, want owner rwx so RemoveAll can descend", di.Mode())
		}
	})

	t.Run("never chmods through an escaping symlink", func(t *testing.T) {
		base := t.TempDir()
		staging := filepath.Join(base, "staging")
		mustSetup(t, os.Mkdir(staging, 0o700))

		victim := filepath.Join(base, "victim")
		mustSetup(t, os.Mkdir(victim, 0o000)) // would be widened to 0700 if the link were followed
		t.Cleanup(func() { _ = os.Chmod(victim, 0o700) })

		mustSetup(t, os.Symlink(victim, filepath.Join(staging, "escape")))

		chownStagingForCleanup(staging, uid, gid)

		// Root confinement must leave the external victim untouched.
		if di := lstat(t, victim); di.Mode().Perm() != 0o000 {
			t.Errorf("external victim mode = %v, want unchanged 0000 (link not followed)", di.Mode())
		}
	})

	t.Run("recurses into non-UTF-8 directory names", func(t *testing.T) {
		base := t.TempDir()
		staging := filepath.Join(base, "staging")
		mustSetup(t, os.Mkdir(staging, 0o700))
		// Raw ReadDir must traverse non-UTF-8 Unix names that io/fs rejects.
		weird := filepath.Join(staging, "d\xff\xfe")
		if err := os.Mkdir(weird, 0o700); err != nil {
			t.Skipf("filesystem rejects non-UTF-8 names (%v); nothing to test", err)
		}
		inner := filepath.Join(weird, "inner")
		mustSetup(t, os.Mkdir(inner, 0o700))
		mustSetup(t, os.Chmod(inner, 0o500)) // widened only if the walk descended

		chownStagingForCleanup(staging, uid, gid)

		if di := lstat(t, inner); di.Mode().Perm()&0o700 != 0o700 {
			t.Errorf("grandchild under a non-UTF-8 dir mode = %v, want owner rwx — the walk must descend by raw bytes", di.Mode())
		}
	})
}

// TestOSRootRejectsEscapingMutations pins non-racy symlink escape rejection for
// Chmod and WriteFile. GO-2026-4864's Linux Root.Chmod target-symlink race is
// outside this test and requires at least Go 1.25.9 on the 1.25 release line or
// Go 1.26.2 on the 1.26 release line.
func TestOSRootRejectsEscapingMutations(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	mustSetup(t, os.Mkdir(root, 0o700))

	victim := filepath.Join(base, "victim")
	mustSetup(t, os.WriteFile(victim, []byte("LIVE\n"), 0o600))

	mustSetup(t, os.Symlink("../victim", filepath.Join(root, "rel"))) // relative escape
	mustSetup(t, os.Symlink(victim, filepath.Join(root, "abs")))      // absolute escape

	rt, err := os.OpenRoot(root)
	mustSetup(t, err)
	defer func() { _ = rt.Close() }()

	for _, name := range []string{"rel", "abs"} {
		if err := rt.Chmod(name, 0o777); err == nil {
			t.Errorf("rt.Chmod(%q) escaped the root, want it rejected", name)
		}
		if err := rt.WriteFile(name, []byte("CLOBBER\n"), 0o600); err == nil {
			t.Errorf("rt.WriteFile(%q) escaped the root, want it rejected", name)
		}
	}
	if data, _ := os.ReadFile(victim); string(data) != "LIVE\n" {
		t.Errorf("external victim content = %q, want unchanged \"LIVE\\n\"", string(data))
	}
	if fi := lstat(t, victim); fi.Mode().Perm() != 0o600 {
		t.Errorf("external victim mode = %v, want unchanged 0600", fi.Mode())
	}
}

// TestMkdirAllOwned uses chown-to-self to verify confined, non-following creation
// without modifying existing ancestors.
func TestMkdirAllOwned(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	base := t.TempDir() // the pre-existing ancestor
	baseMode := lstat(t, base).Mode()

	target := filepath.Join(base, "a", "b", "c")
	if err := mkdirAllOwned(target, uid, gid); err != nil {
		t.Fatalf("mkdirAllOwned: %v", err)
	}
	for _, p := range []string{filepath.Join(base, "a"), filepath.Join(base, "a", "b"), target} {
		if fi := lstat(t, p); !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("created %q = %v, want a 0700 directory", p, fi.Mode())
		}
	}
	if fi := lstat(t, base); fi.Mode() != baseMode {
		t.Errorf("pre-existing ancestor mode = %v, want untouched %v", fi.Mode(), baseMode)
	}
	// Idempotent: the whole chain now exists, so a second call is a no-op success.
	if err := mkdirAllOwned(target, uid, gid); err != nil {
		t.Errorf("mkdirAllOwned (idempotent re-call): %v", err)
	}
	existingFile := filepath.Join(base, "file")
	mustSetup(t, os.WriteFile(existingFile, []byte("x"), 0o600))
	if err := mkdirAllOwned(existingFile, uid, gid); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("mkdirAllOwned(existing file) err = %v, want ENOTDIR", err)
	}
	linkDir := filepath.Join(base, "link-dir")
	mustSetup(t, os.Symlink(target, linkDir))
	if err := mkdirAllOwned(linkDir, uid, gid); err != nil {
		t.Errorf("mkdirAllOwned(symlink to dir): %v", err)
	}
	linkFile := filepath.Join(base, "link-file")
	mustSetup(t, os.Symlink(existingFile, linkFile))
	if err := mkdirAllOwned(linkFile, uid, gid); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("mkdirAllOwned(symlink to file) err = %v, want ENOTDIR", err)
	}
	// Unknown invoker (negative ids) falls back to plain MkdirAll, still creating.
	other := filepath.Join(base, "x", "y")
	if err := mkdirAllOwned(other, -1, -1); err != nil {
		t.Errorf("mkdirAllOwned with unknown invoker: %v", err)
	}
	if fi := lstat(t, other); !fi.IsDir() {
		t.Errorf("unknown-invoker path %q not created", other)
	}
}

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

// trySetXattr reports whether the filesystem supports the test user attribute.
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
	for b := range bytes.SplitSeq(buf[:sz], []byte{0}) {
		if string(b) == name {
			return true
		}
	}
	return false
}
