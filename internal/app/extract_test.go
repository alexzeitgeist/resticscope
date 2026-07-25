package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/config"
)

// longSnapID is a full ID whose prefix matches the request builders' short ID.
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

// fileStagingSetup models a restored file at staging/leaf with distinctive mode
// and mtime so tests detect metadata restamping.
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
		// Defeat umask so metadata assertions test normalization, not process policy.
		if err := os.Chmod(full, 0o640); err != nil {
			return err
		}
		return os.Chtimes(full, fileSetupMtime, fileSetupMtime)
	}
}

// dirStagingSetup models restic creating a rebased directory leaf and optionally
// populating it.
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

// newExtractApp returns an extraction app and its observable cache.
func newExtractApp(r fakeRestic, root string) (*App, *fakeCache) {
	fc := newFakeCache()
	cfg := testConfig()
	cfg.Extract = config.Extract{TargetRoot: root, ExtractTimeout: config.Duration(2 * time.Minute)}
	a := &App{Cfg: cfg, Cache: fc, Clock: fixedClock{now}, Secrets: fakeSecrets{}, Restic: r}
	return a, fc
}

func TestPlanExtractPaths(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	staging, final, err := PlanExtractPaths(cfg, treeReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	wantFinal := "/srv/restore/repo-a/abcd1234/etc/nginx"
	// Staging is repo-level and hashes the raw source path.
	wantStaging := "/srv/restore/repo-a/.resticscope-staging-abcd1234-nginx-2bbac1448fc84276"
	if final != wantFinal {
		t.Errorf("final = %q, want %q", final, wantFinal)
	}
	if staging != wantStaging {
		t.Errorf("staging = %q, want %q", staging, wantStaging)
	}
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

// Repository names are slugged for paths even when upstream validation is bypassed.
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
	if sa != sb || fa != fb {
		t.Errorf("same source produced different paths: staging %q vs %q, final %q vs %q", sa, sb, fa, fb)
	}
	// Only staging needs a source hash; mirror finals are path-distinct.
	if !strings.HasSuffix(sa, "-4a666ea3a3b04a24") {
		t.Errorf("unexpected staging hash for /etc/hosts: %q", sa)
	}
	if fa != "/srv/restore/repo-a/abcd1234/etc/hosts" {
		t.Errorf("final = %q, want the mirror path .../abcd1234/etc/hosts", fa)
	}
	// Equal basenames at different paths must remain distinct.
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

// TestPlanExtractStagingOutsideMirrorTree prevents snapshot content that resembles
// a staging name from colliding with repo-level staging.
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
	if want := repoDir + "/abcd1234/etc/.resticscope-staging-hosts-4a666ea3a3b04a24"; mimicFinal != want {
		t.Errorf("mimic final = %q, want %q (under <short>/)", mimicFinal, want)
	}

	hostsStaging, _, err := PlanExtractPaths(cfg, fileReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths(hosts): %v", err)
	}
	if want := repoDir + "/.resticscope-staging-abcd1234-hosts-4a666ea3a3b04a24"; hostsStaging != want {
		t.Errorf("hosts staging = %q, want %q (repo level)", hostsStaging, want)
	}

	// Mirrored content and real staging must be disjoint.
	if mimicFinal == hostsStaging {
		t.Errorf("mimic final collided with hosts staging: %q", mimicFinal)
	}

	// Every staging path remains a direct repository child.
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

func TestExtractFileRequiresRegularFile(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.WasRegularFile = false
	req.Source = "/etc/topsecret"
	req.SourceName = "topsecret"

	_, err := a.Extract(t.Context(), req, nil)
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
	req.WasRegularFile = true

	_, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "nginx")
}

// All non-regular upstream types share the same file-mode rejection.
func TestExtractFileRejectsNonRegularTypes(t *testing.T) {
	for _, kind := range []string{"symlink", "device", "fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			cap := &extractCapture{}
			a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
			req := fileReq()
			req.WasRegularFile = false
			if _, err := a.Extract(t.Context(), req, nil); !errors.Is(err, ErrExtractInvalidRequest) {
				t.Fatalf("%s: err = %v, want ErrExtractInvalidRequest", kind, err)
			}
			assertNoSpawnNoMkdir(t, cap, root, nil, "")
		})
	}
}

// assertNoSpawnNoMkdir checks that boundary rejection has no side effects or leak.
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

func TestExtractFinalExists(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	_, final, _ := PlanExtractPaths(a.Cfg.Extract, req)
	mustMkdirAll(t, final)

	_, err := a.Extract(t.Context(), req, nil)
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

	_, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractStagingExists) {
		t.Fatalf("err = %v, want ErrExtractStagingExists", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a pre-existing staging dir")
	}
}

// TestExtractFileFlattened checks parent rebasing, raw leaf naming, and restored
// metadata preservation.
func TestExtractFileFlattened(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		sourceName string
	}{
		{"plain basename", "/etc/hosts", "hosts"},
		// Distinguish the raw leaf from its sanitized staging slug.
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

			result, err := a.Extract(t.Context(), req, nil)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			cap.mu.Lock()
			tp := cap.treeParams
			cap.mu.Unlock()
			if tp.Source != path.Dir(tc.source) {
				t.Errorf("treeParams.Source = %q, want %q (parent rebase)", tp.Source, path.Dir(tc.source))
			}
			if want := []string{"/" + leaf}; !slices.Equal(tp.IncludePaths, want) {
				t.Errorf("treeParams.IncludePaths = %q, want %q", tp.IncludePaths, want)
			}
			if tp.Target != staging {
				t.Errorf("treeParams.Target = %q, want %q", tp.Target, staging)
			}

			if result.FinalPath != final {
				t.Errorf("FinalPath = %q, want %q", result.FinalPath, final)
			}
			if want := filepath.Dir(final); result.FinalDir != want {
				t.Errorf("FinalDir = %q, want %q (containing mirror dir)", result.FinalDir, want)
			}
			if result.Files != 1 || result.Dirs != 0 {
				t.Errorf("result counts = {Files:%d Dirs:%d}, want {1 0}", result.Files, result.Dirs)
			}

			// Publish the raw leaf with restored metadata.
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
			if got := filepath.Base(final); got != leaf {
				t.Errorf("final basename = %q, want the raw basename %q", got, leaf)
			}
			if tc.sourceName != leaf && filepath.Base(final) == tc.sourceName {
				t.Errorf("final used the sanitized slug %q as its basename rather than the raw basename %q", tc.sourceName, leaf)
			}
			if _, serr := os.Stat(staging); serr == nil {
				t.Error("staging dir still present after a successful publish")
			}
			if len(fc.saved) != 0 {
				t.Errorf("extract wrote %d cache entries, want 0", len(fc.saved))
			}
		})
	}
}

// TestExtractFileRefusesOccupiedTargetWithoutOverwrite simulates a writer winning
// after FreshTargetCheck and verifies the link publish cannot replace it.
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
		// Create lazy publish ancestors before planting the racing occupant.
		if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
			return err
		}
		return os.WriteFile(final, sentinel, 0o600)
	}
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: setup}, root)

	result, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	got, rerr := os.ReadFile(final)
	if rerr != nil {
		t.Fatalf("sentinel file gone after a refused publish: %v", rerr)
	}
	if string(got) != string(sentinel) {
		t.Errorf("sentinel overwritten: got %q, want %q", got, sentinel)
	}
	if !result.StagingCreated || result.StagingDir != staging {
		t.Errorf("result = {StagingCreated:%v StagingDir:%q}, want {true %q}", result.StagingCreated, result.StagingDir, staging)
	}
	if _, serr := os.Stat(staging); serr != nil {
		t.Errorf("staging not retained after a refused publish: %v", serr)
	}
}

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

	if _, err := a.Extract(t.Context(), req, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := modeOf(t, root); got != 0o755 {
		t.Errorf("target root mode = %o, want 0755 (never chmod'd)", got)
	}
	if got := modeOf(t, repoDir); got != 0o755 {
		t.Errorf("repo dir mode = %o, want 0755 (pre-existing, never chmod'd)", got)
	}
	// Check the new containing directory because final is the restored file.
	if got := modeOf(t, filepath.Dir(final)); got != 0o700 {
		t.Errorf("per-op mirror dir mode = %o, want 0700 (new dir)", got)
	}
}

// TestExtractMergeSharesSnapshotAncestor verifies sibling extracts reuse an
// ancestor without changing it or the first file.
func TestExtractMergeSharesSnapshotAncestor(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}, root)

	hosts := fileReq() // /etc/hosts
	_, hostsFinal, _ := PlanExtractPaths(a.Cfg.Extract, hosts)
	res1, err := a.Extract(t.Context(), hosts, nil)
	if err != nil {
		t.Fatalf("extract /etc/hosts: %v", err)
	}

	a.Restic = fakeRestic{extractTreeSetup: fileStagingSetup("passwd")}
	passwd := fileReq()
	passwd.Source = "/etc/passwd"
	passwd.SourceName = "passwd"
	_, passwdFinal, _ := PlanExtractPaths(a.Cfg.Extract, passwd)
	res2, err := a.Extract(t.Context(), passwd, nil)
	if err != nil {
		t.Fatalf("extract /etc/passwd: %v", err)
	}

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

// TestExtractRefusesParentOfExistingChild verifies a later parent extract cannot
// replace a partial mirror tree.
func TestExtractRefusesParentOfExistingChild(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}, root)

	hosts := fileReq() // /etc/hosts
	_, hostsFinal, _ := PlanExtractPaths(a.Cfg.Extract, hosts)
	if _, err := a.Extract(t.Context(), hosts, nil); err != nil {
		t.Fatalf("extract /etc/hosts: %v", err)
	}
	before, rerr := os.ReadFile(hostsFinal)
	if rerr != nil {
		t.Fatalf("read child: %v", rerr)
	}
	modeBefore := modeOf(t, hostsFinal)

	// Reconstruct the parent after its mirror path became occupied.
	a.Restic = fakeRestic{extractTreeSetup: dirStagingSetup("etc", func(node string) error {
		return os.WriteFile(filepath.Join(node, "newfile"), []byte("x"), 0o644)
	})}
	dir := treeReq()
	dir.Source = "/etc"
	dir.SourceName = "etc"
	if _, err := a.Extract(t.Context(), dir, nil); !errors.Is(err, ErrExtractFinalExists) {
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

// TestExtractRefusesChildOfExistingParent verifies a later child extract cannot
// replace content from an existing parent tree.
func TestExtractRefusesChildOfExistingParent(t *testing.T) {
	root := t.TempDir()
	// Model restic reconstructing the parent leaf and its child.
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: dirStagingSetup("etc", func(node string) error {
		return os.WriteFile(filepath.Join(node, "hosts"), []byte("dir-hosts"), 0o644)
	})}, root)

	dir := treeReq()
	dir.Source = "/etc"
	dir.SourceName = "etc"
	_, dirFinal, _ := PlanExtractPaths(a.Cfg.Extract, dir)
	if _, err := a.Extract(t.Context(), dir, nil); err != nil {
		t.Fatalf("extract /etc: %v", err)
	}
	childPath := filepath.Join(dirFinal, "hosts") // <short>/etc/hosts
	if _, err := os.Lstat(childPath); err != nil {
		t.Fatalf("dir extract did not place the child: %v", err)
	}

	a.Restic = fakeRestic{extractTreeSetup: fileStagingSetup("hosts")}
	file := fileReq() // /etc/hosts
	_, fileFinal, _ := PlanExtractPaths(a.Cfg.Extract, file)
	if fileFinal != childPath {
		t.Fatalf("file final %q != dir child %q (mirror paths should coincide)", fileFinal, childPath)
	}
	if _, err := a.Extract(t.Context(), file, nil); !errors.Is(err, ErrExtractFinalExists) {
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

func TestExtractCancel(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{extractBlock: true}, root)
	req := fileReq()
	staging, _, _ := PlanExtractPaths(a.Cfg.Extract, req)

	ctx, cancel := context.WithCancel(t.Context())
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

	result, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	assertPartialStaging(t, result, staging)
}

// assertPartialStaging checks interrupted output remains owned and unpublished.
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

// A dangling target symlink must count as occupied before staging or restic work
// and return the path-free ErrExtractFinalExists.
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

	_, err := a.Extract(t.Context(), req, nil)
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

// Unclean sources must fail at the application boundary before any side effect.
func TestExtractUncleanSourceRefusedBeforeStaging(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	req := fileReq()
	req.Source = "/etc/../secret"
	req.SourceName = "secret"

	_, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	assertNoSpawnNoMkdir(t, cap, root, err, "secret")
}

func TestExtractLoggingIsPathFree(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	a, _ := newExtractApp(fakeRestic{extractTreeSetup: fileStagingSetup("topsecret")}, root)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))
	req := fileReq()
	req.Source = "/etc/topsecret"
	req.SourceName = "topsecret"

	if _, err := a.Extract(t.Context(), req, nil); err != nil {
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

func TestExtractUnknownRepo(t *testing.T) {
	a, _ := newExtractApp(fakeRestic{}, t.TempDir())
	req := fileReq()
	req.Repo = "nope"
	if _, err := a.Extract(t.Context(), req, nil); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("err = %v, want ErrUnknownRepo (wrapped under extract:)", err)
	}
}

func TestExtractSecretsFailurePropagates(t *testing.T) {
	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)
	a.Secrets = fakeSecrets{err: errors.New("secrets: no repo")}
	if _, err := a.Extract(t.Context(), fileReq(), nil); err == nil {
		t.Fatal("expected the secrets failure to propagate")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.treeCalls != 0 {
		t.Error("restic spawned despite a secrets failure")
	}
}

// Unsupported platforms must fail path-free before staging or restic execution.
func TestExtractUnsupportedPlatformRefusesBeforeSpawn(t *testing.T) {
	orig := liveExtractSupported
	liveExtractSupported = false
	t.Cleanup(func() { liveExtractSupported = orig })

	root := t.TempDir()
	cap := &extractCapture{}
	a, _ := newExtractApp(fakeRestic{extractCap: cap}, root)

	_, err := a.Extract(t.Context(), fileReq(), nil)
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

// TestExtractFreeSpaceWalksUp probes through a nonexistent staging path.
func TestExtractFreeSpaceWalksUp(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "no", "such", "dirs", ".resticscope-staging-x")
	free, known := ExtractFreeSpace(staging)
	if !known {
		t.Fatal("ExtractFreeSpace: known = false on a real filesystem")
	}
	if free <= 0 {
		t.Errorf("free = %d, want > 0 for a writable temp dir", free)
	}
}

// TestFreeBytesAtZeroBlockFS covers the automount trigger shape, where statfs
// succeeds but reports no blocks.
func TestFreeBytesAtZeroBlockFS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no zero-block filesystem at a fixed path off linux")
	}
	if _, known := freeBytesAt("/proc"); known {
		t.Error("freeBytesAt(/proc): known = true, want false")
	}
}

func diffTreeReq() ExtractRequest {
	r := treeReq()
	r.DiffContainer = "diff-abcd1234-00112233"
	r.IncludePaths = []string{"/etc/nginx/conf.d/a.conf", "/etc/nginx/nginx.conf"}
	return r
}

func TestPlanExtractPathsDiffContainer(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	staging, final, err := PlanExtractPaths(cfg, diffTreeReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	// Pair sides publish as sibling snapshot roots inside the container.
	if want := "/srv/restore/repo-a/diff-abcd1234-00112233/abcd1234/etc/nginx"; final != want {
		t.Errorf("final = %q, want %q", final, want)
	}
	// Diff and plain operations use distinct repo-level staging names.
	if dir := filepath.Dir(staging); dir != "/srv/restore/repo-a" {
		t.Errorf("filepath.Dir(staging) = %q, want the repo dir", dir)
	}
	plainStaging, _, err := PlanExtractPaths(cfg, treeReq())
	if err != nil {
		t.Fatalf("PlanExtractPaths(plain): %v", err)
	}
	if staging == plainStaging {
		t.Errorf("diff staging %q must differ from the plain extract's %q", staging, plainStaging)
	}
}

func TestPlanExtractPathsDiffContainerInvalid(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	cases := map[string]string{
		"wrong prefix":           "pair-abcd1234-00112233",
		"equal shorts":           "diff-abcd1234-abcd1234",
		"snapshot not in pair":   "diff-00112233-44556677",
		"uppercase hex":          "diff-ABCD1234-00112233",
		"short component":        "diff-abcd123-00112233",
		"trailing garbage":       "diff-abcd1234-00112233-x",
		"path separator smuggle": "diff-abcd1234-00112233/up",
	}
	for name, container := range cases {
		t.Run(name, func(t *testing.T) {
			req := treeReq()
			req.DiffContainer = container
			if _, _, err := PlanExtractPaths(cfg, req); !errors.Is(err, ErrExtractInvalidRequest) {
				t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
			}
		})
	}
}

func TestPlanExtractPathsIncludeValidation(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}

	ok := diffTreeReq()
	ok.IncludePaths = append(ok.IncludePaths, "/etc/nginx")
	if _, _, err := PlanExtractPaths(cfg, ok); err != nil {
		t.Fatalf("valid include list rejected: %v", err)
	}

	cases := map[string][]string{
		"outside source":      {"/etc/passwd"},
		"sibling name-prefix": {"/etc/nginx2/x"},
		"unclean":             {"/etc/nginx/../nginx/x"},
		"empty entry":         {""},
		"bare root":           {"/"},
		"nul":                 {"/etc/nginx/\x00x"},
	}
	for name, incs := range cases {
		t.Run(name, func(t *testing.T) {
			req := diffTreeReq()
			req.IncludePaths = incs
			if _, _, err := PlanExtractPaths(cfg, req); !errors.Is(err, ErrExtractInvalidRequest) {
				t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
			}
		})
	}

	t.Run("count cap", func(t *testing.T) {
		req := diffTreeReq()
		req.IncludePaths = make([]string, MaxDiffExtractIncludes+1)
		for i := range req.IncludePaths {
			req.IncludePaths[i] = "/etc/nginx/f"
		}
		if _, _, err := PlanExtractPaths(cfg, req); !errors.Is(err, ErrExtractInvalidRequest) {
			t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
		}
	})
	t.Run("byte cap", func(t *testing.T) {
		req := diffTreeReq()
		long := "/etc/nginx/" + strings.Repeat("n", 1024)
		req.IncludePaths = make([]string, (MaxDiffExtractIncludeBytes/len(long))+1)
		for i := range req.IncludePaths {
			req.IncludePaths[i] = long
		}
		if _, _, err := PlanExtractPaths(cfg, req); !errors.Is(err, ErrExtractInvalidRequest) {
			t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
		}
	})
}

// File mode accepts one regular file, not a changed-path list.
func TestExtractFileModeRejectsIncludePaths(t *testing.T) {
	req := fileReq()
	req.IncludePaths = []string{"/etc/hosts"}
	if err := checkExtractModeGate(req); !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
}

// TestExtractTreeParamsDiffIncludes verifies include rebasing beneath the
// reconstructed leaf.
func TestExtractTreeParamsDiffIncludes(t *testing.T) {
	req := diffTreeReq()
	req.IncludePaths = []string{"/etc/nginx", "/etc/nginx/conf.d/a.conf"}
	p := extractTreeParams(req, "/abs/staging")
	if p.Source != "/etc" {
		t.Errorf("Source = %q, want %q", p.Source, "/etc")
	}
	want := []string{"/nginx", "/nginx/conf.d/a.conf"}
	if !slices.Equal(p.IncludePaths, want) {
		t.Errorf("IncludePaths = %q, want %q", p.IncludePaths, want)
	}

	// Root includes remain rooted and unchanged.
	rootReq := treeReq()
	rootReq.Source = "/"
	rootReq.SourceName = "abcd1234"
	rootReq.IncludePaths = []string{"/etc/nginx/x"}
	p = extractTreeParams(rootReq, "/abs/staging")
	if p.Source != "" {
		t.Errorf("root Source = %q, want \"\"", p.Source)
	}
	if want := []string{"/etc/nginx/x"}; !slices.Equal(p.IncludePaths, want) {
		t.Errorf("root IncludePaths = %q, want %q", p.IncludePaths, want)
	}
}

func TestExtractDiffTargetDir(t *testing.T) {
	cfg := config.Extract{TargetRoot: "/srv/restore"}
	dir, err := ExtractDiffTargetDir(cfg, diffTreeReq())
	if err != nil {
		t.Fatalf("ExtractDiffTargetDir: %v", err)
	}
	if want := "/srv/restore/repo-a/diff-abcd1234-00112233"; dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	over := diffTreeReq()
	over.TargetRoot = "/mnt/usb"
	if dir, err = ExtractDiffTargetDir(cfg, over); err != nil || !strings.HasPrefix(dir, "/mnt/usb/") {
		t.Errorf("override dir = %q (err %v), want under /mnt/usb/", dir, err)
	}
	if _, err := ExtractDiffTargetDir(cfg, treeReq()); !errors.Is(err, ErrExtractInvalidRequest) {
		t.Errorf("plain request: err = %v, want ErrExtractInvalidRequest", err)
	}
}

// TestExtractDiffSingleFilePublishesViaLink verifies tree-mode publication of a
// single file and its containing FinalDir.
func TestExtractDiffSingleFilePublishesViaLink(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{
		extractTreeSetup: fileStagingSetup("hosts"),
	}, root)
	req := ExtractRequest{
		Repo:          "repo-a",
		SnapshotID:    longSnapID,
		SnapshotShort: "abcd1234",
		Source:        "/etc/hosts",
		SourceName:    "hosts",
		Mode:          ExtractDirectoryTree, // the diff shape: no node-type attestation
		DiffContainer: "diff-abcd1234-00112233",
		IncludePaths:  []string{"/etc/hosts"},
	}
	staging, final, perr := PlanExtractPaths(a.Cfg.Extract, req)
	if perr != nil {
		t.Fatalf("PlanExtractPaths: %v", perr)
	}
	result, err := a.Extract(t.Context(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fi, statErr := os.Lstat(final)
	if statErr != nil || !fi.Mode().IsRegular() {
		t.Fatalf("final %q: err=%v mode=%v, want a regular file", final, statErr, fi)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging %q should be removed after publish, lstat err = %v", staging, err)
	}
	if result.FinalPath != final {
		t.Errorf("FinalPath = %q, want %q", result.FinalPath, final)
	}
	if want := filepath.Dir(final); result.FinalDir != want {
		t.Errorf("FinalDir = %q, want containing dir %q", result.FinalDir, want)
	}
}

// TestExtractDiffSingleFileRefusesOccupiedFinal checks a mid-run occupant wins.
func TestExtractDiffSingleFileRefusesOccupiedFinal(t *testing.T) {
	root := t.TempDir()
	var final string
	a, _ := newExtractApp(fakeRestic{
		extractTreeSetup: func(target string) error {
			if err := fileStagingSetup("hosts")(target); err != nil {
				return err
			}
			// Occupy final between the fresh-target check and publish.
			if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
				return err
			}
			return os.WriteFile(final, []byte("occupant"), 0o644)
		},
	}, root)
	req := ExtractRequest{
		Repo:          "repo-a",
		SnapshotID:    longSnapID,
		SnapshotShort: "abcd1234",
		Source:        "/etc/hosts",
		SourceName:    "hosts",
		Mode:          ExtractDirectoryTree,
		DiffContainer: "diff-abcd1234-00112233",
		IncludePaths:  []string{"/etc/hosts"},
	}
	var perr error
	_, final, perr = PlanExtractPaths(a.Cfg.Extract, req)
	if perr != nil {
		t.Fatalf("PlanExtractPaths: %v", perr)
	}
	_, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	got, readErr := os.ReadFile(final)
	if readErr != nil || string(got) != "occupant" {
		t.Errorf("occupant must survive untouched, got %q (err %v)", got, readErr)
	}
}

// symlinkStagingSetup models a safe restored symlink selected by a diff row.
func symlinkStagingSetup(leaf, target string) func(staging string) error {
	return func(staging string) error {
		return os.Symlink(target, filepath.Join(staging, leaf))
	}
}

// diffSymlinkReq uses tree mode because diff entries do not attest node type.
func diffSymlinkReq() ExtractRequest {
	return ExtractRequest{
		Repo:          "repo-a",
		SnapshotID:    longSnapID,
		SnapshotShort: "abcd1234",
		Source:        "/etc/cfglink",
		SourceName:    "cfglink",
		Mode:          ExtractDirectoryTree,
		DiffContainer: "diff-abcd1234-00112233",
		IncludePaths:  []string{"/etc/cfglink"},
	}
}

// TestExtractDiffSymlinkLeafPublishes verifies a symlink leaf remains a link and
// uses its containing directory as FinalDir.
func TestExtractDiffSymlinkLeafPublishes(t *testing.T) {
	root := t.TempDir()
	a, _ := newExtractApp(fakeRestic{
		extractTreeSetup: symlinkStagingSetup("cfglink", "actual-config"),
	}, root)
	req := diffSymlinkReq()
	staging, final, perr := PlanExtractPaths(a.Cfg.Extract, req)
	if perr != nil {
		t.Fatalf("PlanExtractPaths: %v", perr)
	}
	result, err := a.Extract(t.Context(), req, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fi, statErr := os.Lstat(final)
	if statErr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("final %q: err=%v mode=%v, want a symlink", final, statErr, fi)
	}
	if tgt, _ := os.Readlink(final); tgt != "actual-config" {
		t.Errorf("published symlink target = %q, want %q", tgt, "actual-config")
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging %q should be removed after publish, lstat err = %v", staging, err)
	}
	if want := filepath.Dir(final); result.FinalDir != want {
		t.Errorf("FinalDir = %q, want containing dir %q", result.FinalDir, want)
	}
}

// TestExtractDiffSymlinkLeafRefusesOccupiedFinal verifies link publication cannot
// replace a mid-run occupant, unlike rename of a symlink.
func TestExtractDiffSymlinkLeafRefusesOccupiedFinal(t *testing.T) {
	root := t.TempDir()
	var final string
	a, _ := newExtractApp(fakeRestic{
		extractTreeSetup: func(staging string) error {
			if err := symlinkStagingSetup("cfglink", "actual-config")(staging); err != nil {
				return err
			}
			// Occupy final between the fresh-target check and publish.
			if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
				return err
			}
			return os.WriteFile(final, []byte("occupant"), 0o644)
		},
	}, root)
	req := diffSymlinkReq()
	var perr error
	_, final, perr = PlanExtractPaths(a.Cfg.Extract, req)
	if perr != nil {
		t.Fatalf("PlanExtractPaths: %v", perr)
	}
	_, err := a.Extract(t.Context(), req, nil)
	if !errors.Is(err, ErrExtractFinalExists) {
		t.Fatalf("err = %v, want ErrExtractFinalExists", err)
	}
	fi, statErr := os.Lstat(final)
	if statErr != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("occupant was replaced by the symlink: err=%v mode=%v", statErr, fi)
	}
	got, readErr := os.ReadFile(final)
	if readErr != nil || string(got) != "occupant" {
		t.Errorf("occupant must survive untouched, got %q (err %v)", got, readErr)
	}
}
