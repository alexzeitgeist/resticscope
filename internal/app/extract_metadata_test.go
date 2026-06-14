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

	// A fifo is a special node: counted under Other, left in place, never an abort.
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
	// restic reconstructs the leaf dir "nginx" at staging/nginx and puts the file
	// inside it (treeReq source is /etc/nginx).
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

	// Both modes now use the rebase-parent + --include shape so restic reconstructs
	// the leaf node (and applies its metadata): Source is the parent, the single
	// include the leaf.
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
	// For a directory extract FinalPath coincides with FinalDir (the mirrored tree
	// root).
	if result.FinalPath != final {
		t.Errorf("FinalPath = %q, want %q", result.FinalPath, final)
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
	// The walk counts the reconstructed leaf dir "nginx" (Dirs=1) plus its file.
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

// TestExtractDirectoryLeafMetadataPreserved pins the fix for the directory leaf
// losing its own metadata. In the mirror tree the extracted directory node IS the
// user's real directory, so it must carry its snapshot mode/mtime — restic
// reconstructs the leaf at staging/<base> via --include (it CREATES the node
// rather than restoring contents into resticscope's pre-made 0700 staging root),
// so the published leaf keeps its (wider) restic mode while the SCAFFOLDING
// ancestor resticscope synthesizes to place it at its mirror path stays private
// 0700. A regression to the contents-rebase shape (the old bug) would publish the
// leaf at 0700 and trip this.
func TestExtractDirectoryLeafMetadataPreserved(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	// restic reconstructs the leaf dir "nginx" (treeReq source /etc/nginx) WITH its
	// snapshot metadata: wider-than-0700 mode, setgid, and an old mtime.
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
	// The published leaf dir keeps its snapshot metadata — NOT resticscope's 0700.
	di := lstat(t, final)
	if di.Mode().Perm() != 0o750 || di.Mode()&os.ModeSetgid == 0 {
		t.Errorf("leaf dir mode = %v, want 0750 with setgid preserved (not 0700)", di.Mode())
	}
	if !di.ModTime().Equal(old) {
		t.Errorf("leaf dir mtime = %v, want preserved %v (not extract time)", di.ModTime(), old)
	}
	// The synthesized scaffolding ancestor stays resticscope's private 0700.
	if pd := lstat(t, filepath.Dir(final)); pd.Mode().Perm() != 0o700 {
		t.Errorf("scaffolding ancestor mode = %v, want 0700 (private)", pd.Mode())
	}
	// The leaf dir itself is counted now: Files=1 (site), Dirs=1 (nginx).
	if result.Files != 1 || result.Dirs != 1 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 1}", result.Files, result.Dirs)
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
		return os.Symlink("/etc/shadow", filepath.Join(node, "leak-link")) // unsafe → counted
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

// --- os.Root confinement: nothing the pass mutates may escape staging ---

// TestNormalizeExtractPlaceholderConfinedToStaging pins the boundary: an unsafe
// symlink to an absolute path OUTSIDE staging yields an in-staging placeholder,
// and the external target is left byte- and mode-identical — the Remove→WriteFile
// never follows the link out of the tree (it goes through the staging *os.Root;
// see applyUnsafeSymlinkPolicy).
func TestNormalizeExtractPlaceholderConfinedToStaging(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "staging")
	mustSetup(t, os.Mkdir(root, 0o700))

	victim := filepath.Join(base, "victim.txt")
	mustSetup(t, os.WriteFile(victim, []byte("LIVE-SECRET\n"), 0o600))

	link := filepath.Join(root, "link")
	mustSetup(t, os.Symlink(victim, link)) // absolute → unsafe

	counts, err := normalizeExtractTreeMetadata(t.Context(), root, unsafeSymlinkPlaceholder)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if counts.UnsafeSymlinks != 1 {
		t.Errorf("UnsafeSymlinks = %d, want 1", counts.UnsafeSymlinks)
	}
	// The placeholder lands IN staging, recording the target, as a 0600 regular file.
	if fi := lstat(t, link); !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("placeholder mode = %v, want a 0600 regular file", fi.Mode())
	}
	if data, _ := os.ReadFile(link); string(data) != victim+"\n" {
		t.Errorf("placeholder contents = %q, want %q", string(data), victim+"\n")
	}
	// The external target is UNTOUCHED — same content, same mode. A followed link
	// would have truncated it to the placeholder text.
	if data, _ := os.ReadFile(victim); string(data) != "LIVE-SECRET\n" {
		t.Errorf("external victim content = %q, want it unchanged", string(data))
	}
	if fi := lstat(t, victim); fi.Mode().Perm() != 0o600 {
		t.Errorf("external victim mode = %v, want unchanged 0600", fi.Mode())
	}
}

// TestChownStagingForCleanup covers the failed-extract cleanup that hands a
// root-restored staging tree back to the invoking user. It runs as the test user
// (chown-to-self is a no-op the kernel permits a non-root owner), so it exercises
// the dir chmod-up and the os.Root confinement without needing real root.
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

		// Restored content that aliases a directory outside staging.
		mustSetup(t, os.Symlink(victim, filepath.Join(staging, "escape")))

		chownStagingForCleanup(staging, uid, gid)

		// The link is not a directory so it is never chmod'd, and the walk is
		// confined to the root, so the external victim keeps its 0000 mode.
		if di := lstat(t, victim); di.Mode().Perm() != 0o000 {
			t.Errorf("external victim mode = %v, want unchanged 0000 (link not followed)", di.Mode())
		}
	})

	t.Run("recurses into non-UTF-8 directory names", func(t *testing.T) {
		base := t.TempDir()
		staging := filepath.Join(base, "staging")
		mustSetup(t, os.Mkdir(staging, 0o700))
		// A restored directory whose name is not valid UTF-8 — legal on Unix and
		// common in backup content. A fs.WalkDir over an io/fs would refuse to
		// recurse into it (paths must be valid UTF-8), stranding its children; the
		// raw-byte walk must descend so they are handed back too.
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

// TestOSRootRejectsEscapingMutations pins the os.Root guarantee the extract pass
// depends on: a Chmod or WriteFile whose path resolves through a symlink that
// escapes the root is rejected, leaving the external target untouched. The Chmod
// dir→symlink TOCTOU additionally needs go1.25.9+ (GO-2026-4864) but isn't
// statically reproducible; this pins the non-racy escape rejection.
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

// TestMkdirAllOwned covers the ownership-preserving MkdirAll: it creates the whole
// missing chain below an existing ancestor, leaves the ancestor untouched, and is
// idempotent. It runs as the test user (chown-to-self is permitted); the swap-race
// it resists is closed structurally — os.Root confinement plus a non-following Lchown.
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
	for b := range bytes.SplitSeq(buf[:sz], []byte{0}) {
		if string(b) == name {
			return true
		}
	}
	return false
}
