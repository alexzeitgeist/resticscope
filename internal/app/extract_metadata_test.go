//go:build linux || darwin

package app

import (
	"bytes"
	"context"
	"errors"
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

// --- direct normalizer unit test ---

func TestNormalizeExtractTreeMetadata(t *testing.T) {
	root := t.TempDir()
	baseline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	file := filepath.Join(root, "file")
	mustSetup(t, os.WriteFile(file, []byte("hello"), 0o666))
	mustSetup(t, os.Chmod(file, 0o666|os.ModeSetuid)) // permissive + suid
	sub := filepath.Join(root, "sub")
	mustSetup(t, os.Mkdir(sub, 0o777))
	mustSetup(t, os.Chmod(sub, 0o777|os.ModeSticky)) // permissive + sticky
	nested := filepath.Join(sub, "nested")
	mustSetup(t, os.WriteFile(nested, []byte("x"), 0o644))
	link := filepath.Join(root, "link")
	mustSetup(t, os.Symlink("sub/nested", link)) // relative, non-escaping

	xattrSet := trySetXattr(t, file)

	// Backdate mtimes so the baseline normalization is observable.
	for _, p := range []string{file, nested, sub} {
		mustSetup(t, os.Chtimes(p, old, old))
	}
	// Backdate the symlink's OWN mtime too (Lutimes is no-follow, unlike Chtimes,
	// which would re-time the target instead).
	tvOld := unix.NsecToTimeval(old.UnixNano())
	mustSetup(t, unix.Lutimes(link, []unix.Timeval{tvOld, tvOld}))

	counts, err := normalizeExtractTreeMetadata(context.Background(), root, baseline)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if counts.Files != 2 || counts.Dirs != 1 {
		t.Errorf("counts = %+v, want Files=2 Dirs=1 (root excluded)", counts)
	}

	fi := lstat(t, file)
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("file mode = %v, want 0600 with no setuid", fi.Mode())
	}
	if !fi.ModTime().Equal(baseline) {
		t.Errorf("file mtime = %v, want baseline %v", fi.ModTime(), baseline)
	}

	di := lstat(t, sub)
	if di.Mode().Perm() != 0o700 || di.Mode()&os.ModeSticky != 0 {
		t.Errorf("dir mode = %v, want 0700 with no sticky", di.Mode())
	}
	if !di.ModTime().Equal(baseline) {
		t.Errorf("dir mtime = %v, want baseline %v", di.ModTime(), baseline)
	}

	if ni := lstat(t, nested); ni.Mode().Perm() != 0o600 {
		t.Errorf("nested file mode = %v, want 0600", ni.Mode())
	}

	if li := lstat(t, link); li.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was not preserved as a symlink (must not be followed)")
	}
	if li := lstat(t, link); !li.ModTime().Equal(baseline) {
		t.Errorf("symlink mtime = %v, want baseline %v (no-follow Lutimes)", li.ModTime(), baseline)
	}

	if xattrSet && hasXattr(t, file, "user.resticscope_test") {
		t.Error("xattr was not removed from the regular file")
	}
}

// A directory restored without owner r/x (e.g. mode 0000) must still normalize:
// the pass forces it owner-traversable before reading its children, then leaves
// the final 0700. Without the pre-read chmod, ReadDir would fail with EACCES and
// the whole extract would be lost.
func TestNormalizeExtractTreeMetadataRestrictiveDir(t *testing.T) {
	root := t.TempDir()
	baseline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	child := filepath.Join(root, "locked")
	mustSetup(t, os.Mkdir(child, 0o700))
	mustSetup(t, os.WriteFile(filepath.Join(child, "inner"), []byte("x"), 0o600))
	mustSetup(t, os.Chmod(child, 0o000)) // no owner r/x
	t.Cleanup(func() { _ = os.Chmod(child, 0o700) })

	counts, err := normalizeExtractTreeMetadata(context.Background(), root, baseline)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if di := lstat(t, child); di.Mode().Perm() != 0o700 {
		t.Errorf("restrictive dir mode = %v, want 0700", di.Mode())
	}
	if fi := lstat(t, filepath.Join(child, "inner")); fi.Mode().Perm() != 0o600 {
		t.Errorf("inner file mode = %v, want 0600", fi.Mode())
	}
	if counts.Files != 1 || counts.Dirs != 1 {
		t.Errorf("counts = %+v, want Files=1 Dirs=1", counts)
	}
}

func TestNormalizeExtractTreeMetadataRejects(t *testing.T) {
	baseline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("absolute symlink", func(t *testing.T) {
		root := t.TempDir()
		mustSetup(t, os.Symlink("/etc/passwd", filepath.Join(root, "bad")))
		assertNormalizeRejected(t, root, baseline)
	})
	t.Run("escaping symlink", func(t *testing.T) {
		root := t.TempDir()
		mustSetup(t, os.Symlink("../../../etc", filepath.Join(root, "bad")))
		assertNormalizeRejected(t, root, baseline)
	})
	t.Run("fifo special node", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		assertNormalizeRejected(t, root, baseline)
	})
}

func assertNormalizeRejected(t *testing.T, root string, baseline time.Time) {
	t.Helper()
	_, err := normalizeExtractTreeMetadata(context.Background(), root, baseline)
	if !errors.Is(err, ErrExtractMetadataNormalization) {
		t.Fatalf("err = %v, want ErrExtractMetadataNormalization", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("normalize error leaked the path: %q", err.Error())
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
	if cap.treeParams.Target != staging || cap.treeParams.DryRun || cap.treeParams.Source != req.Source {
		t.Errorf("tree params = %+v, want Target=%q DryRun=false Source=%q", cap.treeParams, staging, req.Source)
	}
	cap.mu.Unlock()

	if result.FinalDir != final {
		t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
	}
	// The normalizer ran before the rename: the restored file is now 0600 with no
	// setgid bit, under the final dir.
	fi := lstat(t, filepath.Join(final, "nginx.conf"))
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSetgid != 0 {
		t.Errorf("normalized file mode = %v, want 0600 with no setgid", fi.Mode())
	}
	if result.Files != 1 || result.Dirs != 0 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 0} from the normalizer walk", result.Files, result.Dirs)
	}
	if _, serr := os.Stat(staging); serr == nil {
		t.Error("staging dir still present after a successful rename")
	}
	if len(fc.saved) != 0 {
		t.Errorf("tree extract wrote %d cache entries, want 0", len(fc.saved))
	}
}

func TestExtractDirectoryTreeNormalizationFailureLeavesStaging(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, ".probe")
	if err := syscall.Mkfifo(probe, 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	mustSetup(t, os.Remove(probe))

	setup := func(target string) error {
		return syscall.Mkfifo(filepath.Join(target, "pipe"), 0o644) // an unsupported node type
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)
	req := treeReq()
	req.Source = "/etc/leaky-dir"
	req.SourceName = "leaky-dir"
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractMetadataNormalization) {
		t.Fatalf("err = %v, want ErrExtractMetadataNormalization", err)
	}
	if strings.Contains(err.Error(), "leaky-dir") || strings.Contains(err.Error(), root) {
		t.Errorf("normalization error leaked a path: %q", err.Error())
	}
	if _, serr := os.Stat(staging); serr != nil {
		t.Errorf("staging dir not left in place after a normalization failure: %v", serr)
	}
	if _, serr := os.Stat(final); serr == nil {
		t.Error("final dir created despite a normalization failure")
	}
}

func TestExtractTreeLoggingIsPathFree(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	setup := func(target string) error {
		return os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
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
// xattr-removal assertion) when the filesystem does not support it.
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
