package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/browsedb"
	"github.com/alexzeitgeist/resticscope/internal/cache"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// setup creates a config and cache with one repository per seeded state.
func setup(t *testing.T, states map[string]model.RepoState) string {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	var repos strings.Builder
	c := cache.New(cacheDir)
	for name, st := range states {
		st.Name = name
		if err := c.Save(t.Context(), name, st); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
		fmt.Fprintf(&repos, "\n[repos.%s]\ncredential=\"cred-a\"\nendpoint=\"https://fsn1.example.com\"\nregion=\"fsn1\"\nbucket=%q\nexpected_frequency=\"24h\"\n", name, name+"-bucket")
	}

	cfg := fmt.Sprintf(`
[global]
secrets_command = "true"
cache_dir = %q
%s`, cacheDir, repos.String())

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func runStatus(t *testing.T, cfgPath string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"status", "--config", cfgPath}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestStatusExitCodes(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		states   map[string]model.RepoState
		wantCode int
	}{
		{
			name: "all green",
			states: map[string]model.RepoState{
				"repo-a": {RefreshedAt: now, LastSnapshot: now.Add(-2 * time.Hour)},
			},
			wantCode: 0,
		},
		{
			name: "amber yields 1",
			states: map[string]model.RepoState{
				"repo-a": {RefreshedAt: now, LastSnapshot: now.Add(-30 * time.Hour)}, // >24h, within +12h grace
			},
			wantCode: 1,
		},
		{
			name: "red yields 2",
			states: map[string]model.RepoState{
				"repo-a": {RefreshedAt: now, LastSnapshot: now.Add(-72 * time.Hour)},
			},
			wantCode: 2,
		},
		{
			name: "error yields 2",
			states: map[string]model.RepoState{
				"repo-a": {RefreshedAt: now, LastError: "restic snapshots: wrong password (exit 12)"},
			},
			wantCode: 2,
		},
		{
			name:     "missing cache (grey) yields 2",
			states:   map[string]model.RepoState{},
			wantCode: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := tt.states
			cfgPath := setup(t, states)
			if tt.name == "missing cache (grey) yields 2" {
				appendRepoWithoutCache(t, cfgPath)
			}
			code, out, errOut := runStatus(t, cfgPath)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stdout=%q stderr=%q)", code, tt.wantCode, out, errOut)
			}
		})
	}
}

func appendRepoWithoutCache(t *testing.T, cfgPath string) {
	t.Helper()
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "\n[repos.cold-repo]\ncredential=\"cred-a\"\nendpoint=\"https://fsn1.example.com\"\nbucket=\"cold-bucket\"\nexpected_frequency=\"24h\"\n")
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", cfgPath, err)
	}
}

func TestStatusOutputFormat(t *testing.T) {
	now := time.Now()
	cfgPath := setup(t, map[string]model.RepoState{
		"homeserver-system": {
			RefreshedAt:   now,
			LastSnapshot:  now.Add(-8 * time.Hour),
			SnapshotCount: 240,
		},
	})
	code, out, _ := runStatus(t, cfgPath)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "homeserver-system") || !strings.Contains(out, "green") {
		t.Errorf("missing repo/status in output: %q", out)
	}
	if !strings.Contains(out, "240 snaps") {
		t.Errorf("expected snapshot count, got %q", out)
	}
	if !strings.Contains(out, "8h ago") {
		t.Errorf("expected relative time, got %q", out)
	}
}

func TestCachePruneRemovesOrphans(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"live-repo": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	cacheDir := filepath.Dir(cfgPath) + "/cache"
	liveDir := seedResticCache(t, cacheDir, "live-repo")
	orphanDir := seedResticCache(t, cacheDir, "removed-repo")

	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"cache", "prune", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("cache prune exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "removed-repo") || !strings.Contains(s, "orphan") {
		t.Errorf("expected orphan reported as pruned, got %q", s)
	}
	if !strings.Contains(s, "freed") {
		t.Errorf("expected a freed summary, got %q", s)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan cache should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("live cache should be kept, stat err = %v", err)
	}
}

func TestCachePruneDryRun(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"live-repo": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	cacheDir := filepath.Dir(cfgPath) + "/cache"
	orphanDir := seedResticCache(t, cacheDir, "removed-repo")

	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"cache", "prune", "--dry-run", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("cache prune --dry-run exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "would free") {
		t.Errorf("expected conditional wording in dry run, got %q", out.String())
	}
	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("dry run must not delete the orphan cache, stat err = %v", err)
	}
}

func TestCacheUnknownSubcommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(t.Context(), []string{"cache", "frobnicate"}, &out, &errBuf); code != 2 {
		t.Errorf("unknown cache subcommand exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown cache subcommand") {
		t.Errorf("expected error message, got %q", errBuf.String())
	}
}

func TestBrowseStoreCloseRemovesSessionDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "browse-session-test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := browsedb.LockSession(dir)
	if err != nil {
		t.Fatalf("LockSession: %v", err)
	}
	db, err := browsedb.Open(filepath.Join(dir, "db.sqlite"), make([]byte, 32), 0)
	if err != nil {
		_ = lock.Close()
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &browseStore{db: db, lock: lock, dir: dir}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir survived Close, stat err = %v", err)
	}
}

// TestBrowseStoreCloseErrorIsPathFree verifies removal errors do not expose the
// session directory. A read-only parent triggers the failure only for non-root
// users, so successful removal skips the test.
func TestBrowseStoreCloseErrorIsPathFree(t *testing.T) {
	parent := t.TempDir()
	const secret = "SECRET_session_dir_marker"
	dir := filepath.Join(parent, secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := browsedb.LockSession(dir)
	if err != nil {
		t.Fatalf("LockSession: %v", err)
	}
	db, err := browsedb.Open(filepath.Join(dir, "db.sqlite"), make([]byte, 32), 0)
	if err != nil {
		_ = lock.Close()
		t.Fatalf("Open: %v", err)
	}
	store := &browseStore{db: db, lock: lock, dir: dir}

	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) // let t.TempDir cleanup succeed

	err = store.Close()
	if err == nil {
		t.Skip("RemoveAll succeeded despite read-only parent (running as root?); path-free branch not exercised")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Close error leaked the session dir name %q: %v", secret, err)
	}
}

// seedResticCache creates a populated per-repository cache and returns its path.
func seedResticCache(t *testing.T, cacheDir, repoName string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "restic-cache", repoName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSecretsTemplate verifies hermetic scaffolding: stdout contains pipeable
// JSON with blank values, while guidance goes to stderr.
func TestSecretsTemplate(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"repo-a": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
		"repo-b": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"secrets", "template", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("secrets template exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}

	var doc struct {
		Credentials map[string]map[string]string `json:"credentials"`
		Repos       map[string]map[string]string `json:"repos"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}

	cred, ok := doc.Credentials["cred-a"]
	if !ok {
		t.Fatalf("missing credential cred-a:\n%s", out.String())
	}
	if cred["access_key"] != "" || cred["secret_key"] != "" {
		t.Errorf("credential values not blank: %+v", cred)
	}
	for _, name := range []string{"repo-a", "repo-b"} {
		entry, ok := doc.Repos[name]
		if !ok {
			t.Errorf("missing repo %q:\n%s", name, out.String())
			continue
		}
		if entry["restic_password"] != "" {
			t.Errorf("repo %q password not blank: %q", name, entry["restic_password"])
		}
	}

	if strings.Contains(out.String(), "secrets backend") {
		t.Errorf("guidance leaked into stdout: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "resticscope check") {
		t.Errorf("expected a hint on stderr, got %q", errBuf.String())
	}
}

// TestSecretsTemplateEnvShape verifies generic URLs scaffold an environment map
// rather than S3 key fields.
func TestSecretsTemplateEnvShape(t *testing.T) {
	dir := t.TempDir()
	cfg := `
[global]
secrets_command = "true"

[repos.nas-offsite]
url = "b2:nas-backups:repo"
credential = "nas-b2"
expected_frequency = "24h"
`
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"secrets", "template", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("secrets template exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	var doc struct {
		Credentials map[string]struct {
			AccessKey *string           `json:"access_key"`
			Env       map[string]string `json:"env"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	cred, ok := doc.Credentials["nas-b2"]
	if !ok {
		t.Fatalf("missing credential nas-b2:\n%s", out.String())
	}
	if cred.AccessKey != nil {
		t.Errorf("url-form credential must not scaffold the s3 shorthand:\n%s", out.String())
	}
	if cred.Env == nil || len(cred.Env) != 0 {
		t.Errorf("expected an empty env map to fill in, got %v", cred.Env)
	}
}

func TestSecretsTemplateBadConfig(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"secrets", "template", "--config", filepath.Join(t.TempDir(), "nope.toml")}, &out, &errBuf)
	if code != 2 {
		t.Errorf("bad config exit = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
}

func TestSecretsTemplateRejectsArgs(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"repo-a": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"secrets", "template", "--config", cfgPath, "extra"}, &out, &errBuf)
	if code != 2 {
		t.Errorf("extra arg exit = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("extra arg should not print template JSON, got stdout=%q", out.String())
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("expected unexpected argument error, got %q", errBuf.String())
	}
}

func TestSecretsUnknownSubcommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(t.Context(), []string{"secrets", "frobnicate"}, &out, &errBuf); code != 2 {
		t.Errorf("unknown secrets subcommand exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown secrets subcommand") {
		t.Errorf("expected error message, got %q", errBuf.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Errorf("expected error message, got %q", errBuf.String())
	}
}

func TestHelpShowsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(t.Context(), []string{"help"}, &out, &errBuf); code != 0 {
		t.Errorf("help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage, got %q", out.String())
	}
}

// TestTUIFailsFastWhenSecretsUnavailable verifies unusable secrets fail before
// Bubble Tea opens a terminal. setup's empty "true" output makes refreshDeps fail.
func TestTUIFailsFastWhenSecretsUnavailable(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"repo-a": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"tui", "--config", cfgPath}, &out, &errBuf)
	if code != 2 {
		t.Errorf("tui exit = %d, want 2 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "startup failed") {
		t.Errorf("expected startup failure message, got %q", errBuf.String())
	}
}

func TestCheckBadConfig(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"check", "--config", filepath.Join(t.TempDir(), "nope.toml")}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("check exit = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "config") || !strings.Contains(out.String(), "FAILED") {
		t.Errorf("expected a failed config stage, got %q", out.String())
	}
}

// TestCheckFailsAtSecretsStage uses setup's empty secrets output to exercise
// staged exit 2 without spawning restic.
func TestCheckFailsAtSecretsStage(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"repo-a": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	var out, errBuf bytes.Buffer
	code := run(t.Context(), []string{"check", "--config", cfgPath}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("check exit = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "config") || !strings.Contains(s, "ok") {
		t.Errorf("expected config stage to pass, got %q", s)
	}
	if !strings.Contains(s, "secrets") || !strings.Contains(s, "FAILED") {
		t.Errorf("expected secrets stage to fail, got %q", s)
	}
}

// TestCheckResticGateSkipsProbes verifies unsupported or unparseable restic
// versions stop repository probes whose results would be untrustworthy.
func TestCheckResticGateSkipsProbes(t *testing.T) {
	tests := []struct {
		name    string
		version string
		verErr  error
		want    int
	}{
		{"too old", "0.16.0", nil, 1},
		{"unparseable", "not-a-version", nil, 1},
		{"binary missing", "", errors.New("restic binary not found on PATH"), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			version := func(context.Context) (string, error) { return tt.version, tt.verErr }
			probe := func(context.Context) ([]app.RepoCheck, error) { probed = true; return nil, nil }

			var out, errBuf bytes.Buffer
			code := checkRestic(t.Context(), &out, &errBuf, version, probe)
			if code != tt.want {
				t.Errorf("exit = %d, want %d (stdout=%q stderr=%q)", code, tt.want, out.String(), errBuf.String())
			}
			if probed {
				t.Error("repositories must not be probed against an unsupported restic")
			}
			if !strings.Contains(out.String(), "restic") || !strings.Contains(out.String(), "FAILED") {
				t.Errorf("expected a failed restic stage, got %q", out.String())
			}
		})
	}
}

// TestCheckResticCancelledProbeIsExit2 verifies a probe error makes the check
// incomplete even when every repository row appears healthy.
func TestCheckResticCancelledProbeIsExit2(t *testing.T) {
	version := func(context.Context) (string, error) { return "0.18.1", nil }
	probe := func(context.Context) ([]app.RepoCheck, error) {
		return []app.RepoCheck{{Name: "repo-a"}}, context.Canceled
	}
	var out, errBuf bytes.Buffer
	code := checkRestic(t.Context(), &out, &errBuf, version, probe)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stdout=%q)", code, out.String())
	}
	if strings.Contains(out.String(), "all checks passed") {
		t.Errorf("a cancelled check must not report success, got %q", out.String())
	}
}

func TestCheckResticVerdicts(t *testing.T) {
	version := func(context.Context) (string, error) { return "0.18.1", nil }

	allOK := func(context.Context) ([]app.RepoCheck, error) {
		return []app.RepoCheck{{Name: "repo-a"}, {Name: "repo-b"}}, nil
	}
	var out, errBuf bytes.Buffer
	if code := checkRestic(t.Context(), &out, &errBuf, version, allOK); code != 0 {
		t.Fatalf("all reachable: exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "all checks passed") {
		t.Errorf("expected success message, got %q", out.String())
	}

	oneFails := func(context.Context) ([]app.RepoCheck, error) {
		return []app.RepoCheck{
			{Name: "repo-a"},
			{Name: "repo-b", Err: errors.New("restic cat: repository does not exist (exit 10)")},
		}, nil
	}
	out.Reset()
	errBuf.Reset()
	if code := checkRestic(t.Context(), &out, &errBuf, version, oneFails); code != 1 {
		t.Errorf("one unreachable: exit = %d, want 1", code)
	}
}

// TestRefreshExitCodeFloorsOnFailure verifies persistence failures force exit 2
// so callers do not mistake a stale cache for a healthy refresh.
func TestRefreshExitCodeFloorsOnFailure(t *testing.T) {
	green := []app.RepoStatus{{Status: model.StatusGreen}, {Status: model.StatusGreen}}
	if got := refreshExitCode(green, nil); got != 0 {
		t.Errorf("all green, no failure = %d, want 0", got)
	}
	if got := refreshExitCode(green, errors.New(`persist "repo-a": disk full`)); got != 2 {
		t.Errorf("all green but save failed = %d, want 2", got)
	}
	amber := []app.RepoStatus{{Status: model.StatusAmber}}
	if got := refreshExitCode(amber, errors.New("boom")); got != 2 {
		t.Errorf("amber + failure = %d, want 2", got)
	}
	if got := refreshExitCode(amber, nil); got != 1 {
		t.Errorf("amber, no failure = %d, want 1", got)
	}
}
