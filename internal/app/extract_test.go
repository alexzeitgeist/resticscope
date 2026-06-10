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

// dirStagingSetup returns an extractTreeSetup that materializes the restored
// directory node at staging/<base> — the way restic reconstructs a rebased subdir
// via --include (it CREATES the leaf node, so its metadata is preserved) — then
// runs build(node) to populate it. base is the RAW source basename. A nil build
// leaves the leaf dir empty.
func dirStagingSetup(base string, build func(node string) error) func(target string) error {
	return func(target string) error {
		node := filepath.Join(target, base)
		if err := os.MkdirAll(node, 0o755); err != nil {
			return err
		}
		if build == nil {
			return nil
		}
		return build(node)
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
	// Pure mirror tree: the source's true path under the per-snapshot dir.
	wantFinal := "/srv/restore/repo-a/abcd1234/etc/nginx"
	// Staging is repo-level (a sibling of the <short>/ snapshot dirs), carrying the
	// short id in its name; the hash is the first 16 hex chars of SHA-256 over the
	// raw source path.
	wantStaging := "/srv/restore/repo-a/.resticscope-staging-abcd1234-nginx-2bbac1448fc84276"
	if final != wantFinal {
		t.Errorf("final = %q, want %q", final, wantFinal)
	}
	if staging != wantStaging {
		t.Errorf("staging = %q, want %q", staging, wantStaging)
	}
	// Staging lives at the repo level, NOT under the <short>/ mirror subtree.
	if dir := filepath.Dir(staging); dir != "/srv/restore/repo-a" {
		t.Errorf("filepath.Dir(staging) = %q, want the repo dir /srv/restore/repo-a", dir)
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
	sa, fa, err := PlanExtractPaths(cfg, r1)
	if err != nil {
		t.Fatal(err)
	}
	sb, fb, err := PlanExtractPaths(cfg, r1)
	if err != nil {
		t.Fatal(err)
	}
	// Same source is deterministic in both staging and final.
	if sa != sb || fa != fb {
		t.Errorf("same source produced different paths: staging %q vs %q, final %q vs %q", sa, sb, fa, fb)
	}
	// The hash now lives only on the (repo-level) staging name; finals are
	// path-distinct by the mirror layout.
	if !strings.HasSuffix(sa, "-4a666ea3a3b04a24") {
		t.Errorf("unexpected staging hash for /etc/hosts: %q", sa)
	}
	if fa != "/srv/restore/repo-a/abcd1234/etc/hosts" {
		t.Errorf("final = %q, want the mirror path .../abcd1234/etc/hosts", fa)
	}
	// A different source with the same basename hashes differently (staging) and is
	// path-distinct (final).
	r2 := fileReq()
	r2.Source = "/var/backups/hosts"
	sc, fc, err := PlanExtractPaths(cfg, r2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sc, "-1190c10e93ffd94a") {
		t.Errorf("unexpected staging hash for /var/backups/hosts: %q", sc)
	}
	if fc != "/srv/restore/repo-a/abcd1234/var/backups/hosts" {
		t.Errorf("final = %q, want the mirror path .../abcd1234/var/backups/hosts", fc)
	}
	if fa == fc {
		t.Error("two distinct sources produced the same mirror final")
	}
	if sa == sc {
		t.Error("two distinct sources with the same basename collided in staging")
	}
}

// TestPlanExtractStagingOutsideMirrorTree is the regression for the namespace
// finding: staging dirs live at the repo level, never inside the <short>/ mirror
// subtree, so a real snapshot path that mimics a staging name can never collide
// with a future staging path. A source named like a staging dir mirrors under
// <short>/etc/…; the staging computed for a real sibling stays a direct child of
// the repo dir.
func TestPlanExtractStagingOutsideMirrorTree(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	const repoDir = "/srv/restore/repo-a"

	mimic := fileReq()
	mimic.Source = "/etc/.resticscope-staging-hosts-4a666ea3a3b04a24"
	name, err := SanitizeExtractSlug(path.Base(mimic.Source))
	if err != nil {
		t.Fatalf("SanitizeExtractSlug: %v", err)
	}
	mimic.SourceName = name
	mimicStaging, mimicFinal, err := PlanExtractPaths(cfg, mimic)
	if err != nil {
		t.Fatalf("PlanExtractPaths(mimic): %v", err)
	}
	// The mimicking source mirrors under <short>/, not at the repo level.
	if want := repoDir + "/abcd1234/etc/.resticscope-staging-hosts-4a666ea3a3b04a24"; mimicFinal != want {
		t.Errorf("mimic final = %q, want %q (under <short>/)", mimicFinal, want)
	}

	// The staging dir computed for the real /etc/hosts.
	hostsStaging, _, err := PlanExtractPaths(cfg, fileReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths(hosts): %v", err)
	}
	if want := repoDir + "/.resticscope-staging-abcd1234-hosts-4a666ea3a3b04a24"; hostsStaging != want {
		t.Errorf("hosts staging = %q, want %q (repo level)", hostsStaging, want)
	}

	// The mimic's mirror final and the real source's repo-level staging are
	// disjoint: no ErrExtractStagingExists cross-collision is possible.
	if mimicFinal == hostsStaging {
		t.Errorf("mimic final collided with hosts staging: %q", mimicFinal)
	}

	// Equivalent invariant: every staging is a direct child of repoDir and never
	// carries <short> as a path component below repoDir.
	for _, st := range []string{mimicStaging, hostsStaging} {
		if dir := filepath.Dir(st); dir != repoDir {
			t.Errorf("staging %q is not a direct child of repoDir %q", st, repoDir)
		}
		if strings.Contains(st, "/abcd1234/") {
			t.Errorf("staging %q descends through the <short>/ mirror subtree", st)
		}
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
			if tp.Target != staging {
				t.Errorf("treeParams.Target = %q, want %q", tp.Target, staging)
			}

			// The file lands AT its mirror path: FinalPath is the node itself,
			// FinalDir its containing mirror dir.
			if result.FinalPath != final {
				t.Errorf("FinalPath = %q, want %q", result.FinalPath, final)
			}
			if want := filepath.Dir(final); result.FinalDir != want {
				t.Errorf("FinalDir = %q, want %q (containing mirror dir)", result.FinalDir, want)
			}
			if result.Files != 1 || result.Dirs != 0 {
				t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 0}", result.Files, result.Dirs)
			}

			// The file lands AT final (the mirror path, named for the RAW basename),
			// with restic's restored metadata (0640, original mtime) preserved.
			fi, serr := os.Lstat(final)
			if serr != nil {
				t.Fatalf("file not present at its mirror path: %v", serr)
			}
			if fi.Mode().Perm() != 0o640 {
				t.Errorf("published mode = %v, want 0640 preserved (not 0600)", fi.Mode())
			}
			if !fi.ModTime().Equal(fileSetupMtime) {
				t.Errorf("published mtime = %v, want preserved %v (not now)", fi.ModTime(), fileSetupMtime)
			}
			// The mirror path uses the RAW basename, never the sanitized slug.
			if got := filepath.Base(final); got != leaf {
				t.Errorf("final basename = %q, want the raw basename %q", got, leaf)
			}
			if tc.sourceName != leaf && filepath.Base(final) == tc.sourceName {
				t.Errorf("final used the sanitized slug %q as its basename rather than the raw basename %q", tc.sourceName, leaf)
			}
			// The hidden staging dir is gone after a successful publish
			// (link + unlink + rmdir).
			if _, serr := os.Stat(staging); serr == nil {
				t.Error("staging dir still present after a successful publish")
			}
			if len(fc.saved) != 0 {
				t.Errorf("extract wrote %d cache entries, want 0", len(fc.saved))
			}
		})
	}
}

// TestExtractFileRefusesOccupiedTargetWithoutOverwrite proves the os.Link
// no-replace guard — not freshTargetCheck or a publish-time Lstat — protects an
// existing file at the target. The custom setup runs inside ExtractTree (after
// freshTargetCheck, before publish): it both materializes the staging file AND
// writes sentinel bytes to final, simulating a concurrent writer that wins the
// TOCTOU race. Because deep mirror ancestors are created lazily in publishExtract,
// the setup must MkdirAll(filepath.Dir(final)) before writing the sentinel so the
// run reaches the os.Link guard rather than failing during setup.
func TestExtractFileRefusesOccupiedTargetWithoutOverwrite(t *testing.T) {
	root := t.TempDir()
	cfg := config.Extract{TargetRoot: root, ExtractTimeout: config.Duration(2 * time.Minute)}
	req := fileReq() // /etc/hosts
	staging, final, perr := PlanExtractPaths(cfg, req)
	if perr != nil {
		t.Fatalf("PlanExtractPaths: %v", perr)
	}

	sentinel := []byte("DO-NOT-CLOBBER")
	setup := func(target string) error {
		if err := fileStagingSetup("hosts")(target); err != nil {
			return err
		}
		// publishExtract builds the deep mirror ancestors lazily, so create them
		// here before planting the sentinel at final.
		if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
			return err
		}
		return os.WriteFile(final, sentinel, 0o600)
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)

	result, err := a.Extract(context.Background(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	// The existing bytes are untouched: os.Link never replaces.
	got, rerr := os.ReadFile(final)
	if rerr != nil {
		t.Fatalf("sentinel file gone after a refused publish: %v", rerr)
	}
	if string(got) != string(sentinel) {
		t.Errorf("sentinel overwritten: got %q, want %q", got, sentinel)
	}
	// Staging is retained for keep/delete.
	if !result.StagingCreated || result.StagingDir != staging {
		t.Errorf("result = {StagingCreated:%v StagingDir:%q}, want {true %q}", result.StagingCreated, result.StagingDir, staging)
	}
	if _, serr := os.Stat(staging); serr != nil {
		t.Errorf("staging not retained after a refused publish: %v", serr)
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
	// final is now a 0640 file (the mirror leaf), so check its containing per-op
	// mirror dir instead — that is the new dir publishExtract creates at 0700.
	if got := modeOf(t, filepath.Dir(final)); got != 0o700 {
		t.Errorf("per-op mirror dir mode = %o, want 0700 (new dir)", got)
	}
}

// --- merge / overlap behavior (the headline of the mirror-tree change) ---

// TestExtractMergeSharesSnapshotAncestor: two files from one snapshot under the
// same parent merge into a shared <short>/etc/ ancestor — the merge fills empty
// space, reuses the ancestor (never re-chmod'ing it), and leaves the first file
// untouched.
func TestExtractMergeSharesSnapshotAncestor(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}, root)

	hosts := fileReq() // /etc/hosts
	_, hostsFinal, _ := PlanExtractPaths(a.Cfg.Extract, hosts)
	res1, err := a.Extract(context.Background(), hosts, nil)
	if err != nil {
		t.Fatalf("extract /etc/hosts: %v", err)
	}

	// A sibling file under the same /etc — its own staging materializer.
	a.Restic = fakeRestic{extractTreeSetup: fileStagingSetup("passwd")}
	passwd := fileReq()
	passwd.Source = "/etc/passwd"
	passwd.SourceName = "passwd"
	_, passwdFinal, _ := PlanExtractPaths(a.Cfg.Extract, passwd)
	res2, err := a.Extract(context.Background(), passwd, nil)
	if err != nil {
		t.Fatalf("extract /etc/passwd: %v", err)
	}

	// Both land under the same <short>/etc/ ancestor (mode 0700), reused not
	// recreated.
	shared := filepath.Dir(hostsFinal)
	if filepath.Dir(passwdFinal) != shared {
		t.Errorf("passwd parent = %q, want shared ancestor %q", filepath.Dir(passwdFinal), shared)
	}
	if got := modeOf(t, shared); got != 0o700 {
		t.Errorf("shared ancestor mode = %o, want 0700", got)
	}
	if res1.FinalPath == res2.FinalPath {
		t.Errorf("FinalPath not distinct per file: both %q", res1.FinalPath)
	}
	if res1.FinalPath != hostsFinal || res2.FinalPath != passwdFinal {
		t.Errorf("FinalPath mismatch: got %q/%q, want %q/%q", res1.FinalPath, res2.FinalPath, hostsFinal, passwdFinal)
	}
	// The first file is present and untouched by the second extract.
	body, rerr := os.ReadFile(hostsFinal)
	if rerr != nil {
		t.Fatalf("first file gone after the second extract: %v", rerr)
	}
	if string(body) != "file-body" {
		t.Errorf("first file body = %q, want unchanged", string(body))
	}
	if _, serr := os.Lstat(passwdFinal); serr != nil {
		t.Errorf("second file missing: %v", serr)
	}
}

// TestExtractRefusesParentOfExistingChild: a file extract creates <short>/etc/hosts,
// then extracting the whole /etc directory is refused — its leaf <short>/etc is
// occupied by the partial extract — and the existing child is untouched.
func TestExtractRefusesParentOfExistingChild(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}, root)

	hosts := fileReq() // /etc/hosts
	_, hostsFinal, _ := PlanExtractPaths(a.Cfg.Extract, hosts)
	if _, err := a.Extract(context.Background(), hosts, nil); err != nil {
		t.Fatalf("extract /etc/hosts: %v", err)
	}
	before, rerr := os.ReadFile(hostsFinal)
	if rerr != nil {
		t.Fatalf("read child: %v", rerr)
	}
	modeBefore := modeOf(t, hostsFinal)

	// Now the whole /etc directory: <short>/etc is occupied. restic reconstructs
	// the leaf at staging/etc.
	a.Restic = fakeRestic{extractTreeSetup: dirStagingSetup("etc", func(node string) error {
		return os.WriteFile(filepath.Join(node, "newfile"), []byte("x"), 0o644)
	})}
	dir := treeReq()
	dir.Source = "/etc"
	dir.SourceName = "etc"
	if _, err := a.Extract(context.Background(), dir, nil); !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	after, rerr := os.ReadFile(hostsFinal)
	if rerr != nil {
		t.Fatalf("existing child gone after the refused dir extract: %v", rerr)
	}
	if string(after) != string(before) {
		t.Errorf("existing child body changed: %q -> %q", before, after)
	}
	if got := modeOf(t, hostsFinal); got != modeBefore {
		t.Errorf("existing child mode changed: %o -> %o", modeBefore, got)
	}
}

// TestExtractRefusesChildOfExistingParent: a directory extract creates <short>/etc
// (restic reconstructs the leaf at staging/etc, so staging/etc/hosts becomes
// <short>/etc/hosts after the rename), then extracting the file /etc/hosts is
// refused — its mirror leaf already exists in the tree — and the existing file is
// untouched.
func TestExtractRefusesChildOfExistingParent(t *testing.T) {
	root := t.TempDir()
	// restic reconstructs /etc's node at staging/etc, so staging/etc/hosts becomes
	// <short>/etc/hosts after the rename.
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: dirStagingSetup("etc", func(node string) error {
		return os.WriteFile(filepath.Join(node, "hosts"), []byte("dir-hosts"), 0o644)
	})}, root)

	dir := treeReq()
	dir.Source = "/etc"
	dir.SourceName = "etc"
	_, dirFinal, _ := PlanExtractPaths(a.Cfg.Extract, dir)
	if _, err := a.Extract(context.Background(), dir, nil); err != nil {
		t.Fatalf("extract /etc: %v", err)
	}
	childPath := filepath.Join(dirFinal, "hosts") // <short>/etc/hosts
	if _, err := os.Lstat(childPath); err != nil {
		t.Fatalf("dir extract did not place the child: %v", err)
	}

	// Now the file /etc/hosts: its mirror leaf == the dir's child, already present.
	a.Restic = fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}
	file := fileReq() // /etc/hosts
	_, fileFinal, _ := PlanExtractPaths(a.Cfg.Extract, file)
	if fileFinal != childPath {
		t.Fatalf("file final %q != dir child %q (mirror paths should coincide)", fileFinal, childPath)
	}
	if _, err := a.Extract(context.Background(), file, nil); !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	body, rerr := os.ReadFile(childPath)
	if rerr != nil {
		t.Fatalf("existing child gone: %v", rerr)
	}
	if string(body) != "dir-hosts" {
		t.Errorf("existing child changed to %q, want dir-hosts", string(body))
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

// On a platform where the extract cannot publish (normalizer unvalidated /
// non-Windows-safe include escaping), App.Extract refuses BEFORE any restic
// spawn or staging mkdir, with a path-free error. The test flips the
// linux/darwin liveExtractSupported var to simulate the unsupported case.
func TestExtractUnsupportedPlatformRefusesBeforeSpawn(t *testing.T) {
	orig := liveExtractSupported
	liveExtractSupported = false
	t.Cleanup(func() { liveExtractSupported = orig })

	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)

	// A file extract is refused before restic / staging, path-free.
	_, err := a.Extract(context.Background(), fileReq(), nil)
	if err == nil {
		t.Fatal("expected a refusal on an unsupported platform")
	}
	if strings.Contains(err.Error(), "hosts") || strings.Contains(err.Error(), root) {
		t.Errorf("preflight error leaked a path: %q", err.Error())
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Errorf("restic spawned %d times on an unsupported platform, want 0", cap.treeCalls)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("staging created on an unsupported platform: %d entries", len(ents))
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
