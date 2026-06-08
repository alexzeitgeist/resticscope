package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
)

// longSnapID is a concrete 64-hex snapshot ID for requests. Its first 8 chars are
// "abcd1234" so it binds to the SnapshotShort the builders use (the app layer
// asserts SnapshotShort == SnapshotID[:8]).
const longSnapID = "abcd12340123456789abcdef0123456789abcdef0123456789abcdef01234567"

// treeReq / fileReq are valid baseline requests; tests mutate one field at a time.
func treeReq() ExtractRequest {
	return ExtractRequest{
		Repo:          "repo-a",
		SnapshotID:    longSnapID,
		SnapshotShort: "abcd1234",
		Source:        "/etc/nginx",
		SourceName:    "nginx",
		Mode:          ExtractDirectoryTree,
	}
}

func fileReq() ExtractRequest {
	return ExtractRequest{
		Repo:           "repo-a",
		SnapshotID:     longSnapID,
		SnapshotShort:  "abcd1234",
		Source:         "/etc/hosts",
		SourceName:     "hosts",
		Mode:           ExtractFile,
		WasRegularFile: true,
	}
}

// fileStagingSetup returns an extractTreeSetup that materializes one regular file
// at staging/<leaf> with a distinctive mode+mtime, the way restic restore would
// land a single-file extract. leaf is relative (e.g. "hosts" flattened, or
// "etc/hosts" nested); parent dirs are created as needed. The mode (0640) and
// mtime are deliberately not 0600/now so the test proves restic's metadata is
// preserved rather than re-stamped.
var fileSetupMtime = time.Date(2009, 1, 2, 3, 4, 5, 0, time.UTC)

func fileStagingSetup(leaf string) func(target string) error {
	return func(target string) error {
		full := filepath.Join(target, leaf)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte("file-body"), 0o640); err != nil {
			return err
		}
		// Defeat the process umask so the exact mode bits hold under any umask
		// (a hardened umask 077 would otherwise create 0600, which the normalizer
		// faithfully preserves — failing the 0640 assertion for the wrong reason).
		// Mirrors TestExtractDirectoryTreeRealRun's explicit Chmod.
		if err := os.Chmod(full, 0o640); err != nil {
			return err
		}
		return os.Chtimes(full, fileSetupMtime, fileSetupMtime)
	}
}

// newExtractApp wires an App over a temp target root and returns the cache so a
// test can assert nothing was persisted.
func newExtractApp(r fakeRestic, root string) (*App, *fakeCache) {
	fc := newFakeCache()
	cfg := testConfig()
	cfg.Extract = config.Extract{TargetRoot: root, ExtractTimeout: config.Duration(2 * time.Minute)}
	a := &App{Cfg: cfg, Cache: fc, Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: r}
	return a, fc
}

// --- plan tests ---

func TestPlanExtractPaths(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	staging, final, err := PlanExtractPaths(cfg, treeReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	wantFinal := "/srv/restore/repo-a/abcd1234-nginx-2bbac144"
	wantStaging := "/srv/restore/repo-a/.resticscope-staging-abcd1234-nginx-2bbac144"
	if final != wantFinal {
		t.Errorf("final = %q, want %q", final, wantFinal)
	}
	if staging != wantStaging {
		t.Errorf("staging = %q, want %q", staging, wantStaging)
	}
}

func TestPlanExtractPathsTargetRootOverride(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	req := treeReq()
	req.TargetRoot = "/mnt/usb"
	staging, final, err := PlanExtractPaths(cfg, req)
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	if !strings.HasPrefix(final, "/mnt/usb/") || !strings.HasPrefix(staging, "/mnt/usb/") {
		t.Errorf("override ignored: staging=%q final=%q", staging, final)
	}
}

// A slash-bearing repo name (defense-in-depth; loaded config rejects these) must
// slugify in the path while the raw name is untouched for display.
func TestPlanExtractPathsRepoSlug(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	req := treeReq()
	req.Repo = "home/server"
	_, final, err := PlanExtractPaths(cfg, req)
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	if !strings.HasPrefix(final, "/srv/restore/home-server/") {
		t.Errorf("repo slug not applied: %q", final)
	}
}

func TestPlanExtractPathsHashDeterminismAndCollision(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	r1 := fileReq() // /etc/hosts
	_, a, err := PlanExtractPaths(cfg, r1)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := PlanExtractPaths(cfg, r1)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("same source produced different paths: %q vs %q", a, b)
	}
	if !strings.HasSuffix(a, "-4a666ea3") {
		t.Errorf("unexpected hash for /etc/hosts: %q", a)
	}
	// A different source with the same basename must hash differently.
	r2 := fileReq()
	r2.Source = "/var/backups/hosts"
	_, c, err := PlanExtractPaths(cfg, r2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c, "-1190c10e") {
		t.Errorf("unexpected hash for /var/backups/hosts: %q", c)
	}
	if a == c {
		t.Error("two distinct sources with the same basename collided")
	}
}

func TestPlanExtractPathsInvalid(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	cases := map[string]func(*ExtractRequest){
		"empty repo":               func(r *ExtractRequest) { r.Repo = "" },
		"empty snapshot short":     func(r *ExtractRequest) { r.SnapshotShort = "" },
		"non-hex snapshot short":   func(r *ExtractRequest) { r.SnapshotShort = "ABCD1234" },
		"short snapshot short":     func(r *ExtractRequest) { r.SnapshotShort = "abc" },
		"non-hex snapshot id":      func(r *ExtractRequest) { r.SnapshotID = "g" + r.SnapshotID[1:] },
		"short snapshot id":        func(r *ExtractRequest) { r.SnapshotID = "abcd1234" },
		"short id mismatch":        func(r *ExtractRequest) { r.SnapshotShort = "deadbeef" },
		"empty source":             func(r *ExtractRequest) { r.Source = "" },
		"unclean source":           func(r *ExtractRequest) { r.Source = "/etc/../secret"; r.SourceName = "secret" },
		"nul source":               func(r *ExtractRequest) { r.Source = "/etc/ng\x00inx" },
		"empty source name":        func(r *ExtractRequest) { r.SourceName = "" },
		"source name mismatch":     func(r *ExtractRequest) { r.SourceName = "elsewhere" },
		"source name leading dot":  func(r *ExtractRequest) { r.Source = "/etc/.secret"; r.SourceName = ".secret" },
		"source name has sep":      func(r *ExtractRequest) { r.SourceName = "a/b" },
		"relative target override": func(r *ExtractRequest) { r.TargetRoot = "relative/dir" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := treeReq()
			mutate(&req)
			if _, _, err := PlanExtractPaths(cfg, req); !errors.Is(err, ErrExtractInvalidRequest) {
				t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
			}
		})
	}
}

func TestPlanExtractPathsMissingTargetRoot(t *testing.T) {
	if _, _, err := PlanExtractPaths(config.Extract{}, treeReq()); !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest for an empty configured target root", err)
	}
}

// --- file-type gate (defense in depth) ---

func TestExtractFileRequiresRegularFile(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.WasRegularFile = false // upstream said this is a symlink/device/fifo/socket
	req.Source = "/etc/topsecret"
	req.SourceName = "topsecret"

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "topsecret")
}

func TestExtractFileRejectsDryRun(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.DryRun = true

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest (files have no dry-run flow)", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "hosts")
}

func TestExtractDirectoryTreeRejectsRegularFile(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := treeReq()
	req.WasRegularFile = true // a regular file may not route through restore

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "nginx")
}

// Every non-regular upstream type lands as WasRegularFile=false on an ExtractFile
// request and is rejected identically.
func TestExtractFileRejectsNonRegularTypes(t *testing.T) {
	for _, kind := range []string{"symlink", "device", "fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			cap := &extractCapture{}
			a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
			req := fileReq()
			req.WasRegularFile = false
			if _, err := a.Extract(context.Background(), req, nil); !errors.Is(err, ErrExtractInvalidRequest) {
				t.Fatalf("%s: err = %v, want ErrExtractInvalidRequest", kind, err)
			}
			assertNoSpawnNoMkdir(t, cap, root, nil, "")
		})
	}
}

// assertNoSpawnNoMkdir verifies a boundary rejection spawned no restic, created
// no target subdir, and (when err is supplied) leaked no path.
func assertNoSpawnNoMkdir(t *testing.T, cap *extractCapture, root string, err error, leak string) {
	t.Helper()
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Errorf("restic spawned on a boundary rejection: tree=%d", cap.treeCalls)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("target root not left empty: %d entries", len(ents))
	}
	if err != nil && leak != "" && strings.Contains(err.Error(), leak) {
		t.Errorf("error leaked %q: %q", leak, err.Error())
	}
}

// --- fresh-target check ---

func TestExtractFinalExists(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)
	mustMkdirAll(t, final)

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)
	if _, serr := os.Stat(staging); serr == nil {
		t.Error("staging dir was created despite a pre-existing final dir")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a pre-existing final dir")
	}
}

func TestExtractStagingExists(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)
	mustMkdirAll(t, staging)

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractStagingExists) {
		t.Fatalf("err = %v, want ErrExtractStagingExists", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a pre-existing staging dir")
	}
}

func TestExtractDryRunFreshTargetConflict(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := treeReq()
	req.DryRun = true
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)
	mustMkdirAll(t, final)

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("dry-run err = %v, want ErrExtractFinalExists", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("dry-run spawned restic despite a fresh-target conflict")
	}
}

// --- dry-run dispatch ---

func TestExtractDirectoryTreeDryRun(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{
		extractCap: cap,
		extractTreeEvents: []resticx.ExtractTreeEvent{
			{Kind: resticx.ExtractTreeVerboseStatus, Action: model.RestoreActionRestored, Item: "/etc/nginx/nginx.conf", Size: 120},
			{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 120},
		},
	}, root)
	req := treeReq()
	req.DryRun = true

	result, err := a.Extract(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Extract dry-run: %v", err)
	}
	// No directories are created for a dry-run.
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("dry-run created %d dir(s) under the target root, want 0", len(ents))
	}
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)
	cap.mu.Lock()
	gotTarget := cap.treeParams.Target
	gotDry := cap.treeParams.DryRun
	gotSource := cap.treeParams.Source
	cap.mu.Unlock()
	if gotTarget != staging || !gotDry || gotSource != req.Source {
		t.Errorf("tree params = {Target:%q DryRun:%v Source:%q}, want {%q true %q}", gotTarget, gotDry, gotSource, staging, req.Source)
	}
	if len(result.DryRunPreview) != 1 || result.DryRunPreview[0].Action != model.RestoreActionRestored || result.DryRunPreview[0].Item != "/etc/nginx/nginx.conf" || result.DryRunPreview[0].Size != 120 {
		t.Errorf("DryRunPreview = %+v, want one restored row", result.DryRunPreview)
	}
	if result.FinalDir != "" {
		t.Errorf("FinalDir = %q, want empty for a dry-run", result.FinalDir)
	}
	if result.Files != 1 || result.Bytes != 120 {
		t.Errorf("summary not aggregated: Files=%d Bytes=%d", result.Files, result.Bytes)
	}
}

// --- single-file extract via restore ---

// TestExtractFileFlattened drives the default (flattened) file extract: restic
// restore is called with the parent rebased onto Source and a rebase-relative
// --include, the file lands directly under final named for its RAW basename, and
// the normalizer preserves restic's restored metadata (not the old 0600/now).
func TestExtractFileFlattened(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		sourceName string
	}{
		{"plain basename", "/etc/hosts", "hosts"},
		// A basename whose sanitized slug differs from the raw name: the restored
		// file leaf must be the RAW basename ("a*.conf"), never the slug.
		{"sanitized container slug", "/etc/a*.conf", "a-.conf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cap := &extractCapture{}
			leaf := path.Base(tc.source)
			a, fc := newExtractApp(fakeRestic{
				extractCap:       cap,
				extractTreeSetup: fileStagingSetup(leaf),
			}, root)
			req := fileReq()
			req.Source = tc.source
			req.SourceName = tc.sourceName
			staging, final, perr := PlanExtractPaths(a.Cfg.Extract, req)
			if perr != nil {
				t.Fatalf("PlanExtractPaths: %v", perr)
			}

			result, err := a.Extract(context.Background(), req, nil)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			cap.mu.Lock()
			tp := cap.treeParams
			cap.mu.Unlock()
			if tp.Source != path.Dir(tc.source) {
				t.Errorf("treeParams.Source = %q, want %q (parent rebase)", tp.Source, path.Dir(tc.source))
			}
			if want := "/" + leaf; tp.IncludePath != want {
				t.Errorf("treeParams.IncludePath = %q, want %q", tp.IncludePath, want)
			}
			if tp.Target != staging || tp.DryRun {
				t.Errorf("treeParams = {Target:%q DryRun:%v}, want {%q false}", tp.Target, tp.DryRun, staging)
			}

			if result.FinalDir != final {
				t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
			}
			if result.Files != 1 || result.Dirs != 0 {
				t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 0}", result.Files, result.Dirs)
			}

			// The file lands flattened under final, named for the RAW basename, with
			// restic's restored metadata (0640, original mtime) preserved.
			published := filepath.Join(final, leaf)
			fi, serr := os.Lstat(published)
			if serr != nil {
				t.Fatalf("flattened file not present at final/<raw-basename>: %v", serr)
			}
			if fi.Mode().Perm() != 0o640 {
				t.Errorf("published mode = %v, want 0640 preserved (not 0600)", fi.Mode())
			}
			if !fi.ModTime().Equal(fileSetupMtime) {
				t.Errorf("published mtime = %v, want preserved %v (not now)", fi.ModTime(), fileSetupMtime)
			}
			// The sanitized slug must NOT be used as the file leaf.
			if tc.sourceName != leaf {
				if _, e := os.Lstat(filepath.Join(final, tc.sourceName)); e == nil {
					t.Errorf("file published under the sanitized slug %q rather than the raw basename %q", tc.sourceName, leaf)
				}
			}
			if _, serr := os.Stat(staging); serr == nil {
				t.Error("staging dir still present after a successful rename")
			}
			if len(fc.saved) != 0 {
				t.Errorf("extract wrote %d cache entries, want 0", len(fc.saved))
			}
		})
	}
}

// TestExtractFileNested drives the opt-in nested layout: no source rebase, the
// full source is the include, and restic reconstructs the parent path so the file
// lands at final/etc/hosts. The reconstructed parent dir is counted in Dirs.
func TestExtractFileNested(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{
		extractCap:       cap,
		extractTreeSetup: fileStagingSetup("etc/hosts"),
	}, root)
	req := fileReq()
	req.Nested = true
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	cap.mu.Lock()
	tp := cap.treeParams
	cap.mu.Unlock()
	if tp.Source != "" {
		t.Errorf("nested treeParams.Source = %q, want \"\" (bare snapshot)", tp.Source)
	}
	if tp.IncludePath != req.Source {
		t.Errorf("nested treeParams.IncludePath = %q, want %q", tp.IncludePath, req.Source)
	}

	if result.FinalDir != final {
		t.Errorf("FinalDir = %q, want %q", result.FinalDir, final)
	}
	// The reconstructed "etc" parent dir is counted: Files=1, Dirs=1.
	if result.Files != 1 || result.Dirs != 1 {
		t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 1}", result.Files, result.Dirs)
	}
	if _, serr := os.Lstat(filepath.Join(final, "etc", "hosts")); serr != nil {
		t.Errorf("nested file not present at final/etc/hosts: %v", serr)
	}
	if _, serr := os.Stat(staging); serr == nil {
		t.Error("staging dir still present after a successful rename")
	}
}

// --- target_root is never chmod'd ---

func TestExtractDoesNotChmodTargetRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	mustMkdirMode(t, root, 0o755)
	// A repo dir the user widened deliberately by a prior extract.
	repoDir := filepath.Join(root, "repo-a")
	mustMkdirMode(t, repoDir, 0o755)

	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}, root)
	req := fileReq()
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)

	if _, err := a.Extract(context.Background(), req, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := modeOf(t, root); got != 0o755 {
		t.Errorf("target root mode = %o, want 0755 (never chmod'd)", got)
	}
	if got := modeOf(t, repoDir); got != 0o755 {
		t.Errorf("repo dir mode = %o, want 0755 (pre-existing, never chmod'd)", got)
	}
	if got := modeOf(t, final); got != 0o700 {
		t.Errorf("final dir mode = %o, want 0700 (new per-op dir)", got)
	}
}

// --- cancel / timeout (file mode, now via restore) ---

func TestExtractCancel(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractBlock: true}, root)
	req := fileReq()
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the blocking fake returns ctx.Err() immediately

	result, err := a.Extract(ctx, req, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertPartialStaging(t, result, staging)
}

func TestExtractTimeout(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractBlock: true}, root)
	a.Cfg.Extract.ExtractTimeout = config.Duration(5 * time.Millisecond)
	req := fileReq()
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)

	result, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	assertPartialStaging(t, result, staging)
}

// assertPartialStaging checks the result/disk state shared by cancel and timeout:
// staging created and left on disk, no final.
func assertPartialStaging(t *testing.T, result ExtractResult, staging string) {
	t.Helper()
	if !result.StagingCreated || result.StagingDir != staging {
		t.Errorf("result = {StagingCreated:%v StagingDir:%q}, want {true %q}", result.StagingCreated, result.StagingDir, staging)
	}
	if result.FinalDir != "" {
		t.Errorf("FinalDir = %q, want empty on an interrupted extract", result.FinalDir)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("staging dir not left on disk for keep-or-delete: %v", err)
	}
}

// --- fresh-target: a dangling symlink is an occupant ---

// A dangling symlink at the final path is an occupant: the fresh-target check
// Lstats (it must not follow the link down to ENOENT), so the extract is refused
// before any staging dir or restic spawn — not discovered later as a rename
// failure. The error is the path-free ErrExtractFinalExists.
func TestExtractDanglingSymlinkAtFinalIsRefused(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.Source = "/etc/supersecret-leak"
	req.SourceName = "supersecret-leak"
	staging, final, _ := PlanExtractPaths(a.Cfg.Extract, req)
	mustMkdirAll(t, filepath.Dir(final))
	if err := os.Symlink(filepath.Join(root, "nonexistent-target"), final); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	if strings.Contains(err.Error(), "supersecret-leak") || strings.Contains(err.Error(), root) {
		t.Errorf("fresh-target error leaked a path: %q", err.Error())
	}
	if _, serr := os.Lstat(staging); serr == nil {
		t.Error("staging dir created despite a pre-existing (symlink) final path")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a pre-existing (symlink) final path")
	}
}

// --- boundary: malformed request dies before side effects ---

// An unclean source (resolves elsewhere after cleaning) is refused at the app
// boundary with ErrExtractInvalidRequest — before credentials, staging, or a
// restic spawn — so a malformed request never leaves staging behind nor leans on
// resticx to reject it after filesystem side effects.
func TestExtractUncleanSourceRefusedBeforeStaging(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.Source = "/etc/../secret"
	req.SourceName = "secret"

	_, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "secret")
}

// --- privacy: logging is path-free ---

func TestExtractLoggingIsPathFree(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("topsecret")}, root)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))
	req := fileReq()
	req.Source = "/etc/topsecret"
	req.SourceName = "topsecret"

	if _, err := a.Extract(context.Background(), req, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	logged := buf.String()
	for _, leak := range []string{"topsecret", root} {
		if strings.Contains(logged, leak) {
			t.Errorf("log leaked %q:\n%s", leak, logged)
		}
	}
	if !strings.Contains(logged, "extract.file") || !strings.Contains(logged, "repo-a") {
		t.Errorf("expected count-only finish line, got:\n%s", logged)
	}
}

// --- unknown repo / secrets failure ---

func TestExtractUnknownRepo(t *testing.T) {
	a, _ := newExtractApp(fakeRestic{}, t.TempDir())
	req := fileReq()
	req.Repo = "nope"
	if _, err := a.Extract(context.Background(), req, nil); err == nil {
		t.Fatal("expected an error for an unknown repo")
	}
}

func TestExtractSecretsFailurePropagates(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	a.Secrets = fakeSecrets{err: errors.New("secrets: no repo")}
	if _, err := a.Extract(context.Background(), fileReq(), nil); err == nil {
		t.Fatal("expected the secrets failure to propagate")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a secrets failure")
	}
}

// --- unsupported-platform preflight ---

// On a platform where a live extract cannot publish (normalizer unvalidated /
// non-Windows-safe include escaping), App.Extract refuses a live (non-dry-run)
// extract BEFORE any restic spawn or staging mkdir, with a path-free error —
// while a directory dry-run still proceeds everywhere (it carries no --include
// and returns before staging/normalize). The test flips the linux/darwin
// liveExtractSupported var to simulate the unsupported case.
func TestExtractUnsupportedPlatformRefusesBeforeSpawn(t *testing.T) {
	orig := liveExtractSupported
	liveExtractSupported = false
	t.Cleanup(func() { liveExtractSupported = orig })

	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{
		extractCap: cap,
		extractTreeEvents: []resticx.ExtractTreeEvent{
			{Kind: resticx.ExtractTreeVerboseStatus, Action: model.RestoreActionRestored, Item: "/etc/nginx/nginx.conf", Size: 10},
			{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 10},
		},
	}, root)

	// A live file extract is refused before restic / staging, path-free.
	_, err := a.Extract(context.Background(), fileReq(), nil)
	if err == nil {
		t.Fatal("expected a refusal on an unsupported platform")
	}
	if strings.Contains(err.Error(), "hosts") || strings.Contains(err.Error(), root) {
		t.Errorf("preflight error leaked a path: %q", err.Error())
	}
	cap.mu.Lock()
	liveCalls := cap.treeCalls
	cap.mu.Unlock()
	if liveCalls != 0 {
		t.Errorf("restic spawned %d times on an unsupported platform, want 0", liveCalls)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("staging created on an unsupported platform: %d entries", len(ents))
	}

	// A directory dry-run still proceeds everywhere (returns before staging/normalize).
	dreq := treeReq()
	dreq.DryRun = true
	res, derr := a.Extract(context.Background(), dreq, nil)
	if derr != nil {
		t.Fatalf("directory dry-run must proceed on any platform: %v", derr)
	}
	if len(res.DryRunPreview) != 1 {
		t.Errorf("dry-run preview = %d rows, want 1", len(res.DryRunPreview))
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 1 {
		t.Errorf("treeCalls = %d, want 1 (only the directory dry-run reached restic)", cap.treeCalls)
	}
}

// --- helpers ---

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustMkdirMode(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, mode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil { // defeat umask so the exact bits hold
		t.Fatalf("chmod %s: %v", dir, err)
	}
}

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}
