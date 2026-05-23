package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"resticscope/internal/app"
	"resticscope/internal/cache"
	"resticscope/internal/model"
)

// setup writes a config file and a cache dir, seeds the given repo states, and
// returns the config path. The config declares one repo per seeded state.
func setup(t *testing.T, states map[string]model.RepoState) string {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	var repos strings.Builder
	c := cache.New(cacheDir)
	for name, st := range states {
		st.Name = name
		if err := c.Save(context.Background(), name, st); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
		fmt.Fprintf(&repos, "\n[[repos]]\nname=%q\ncredential=\"cred-a\"\nbucket=%q\nexpected_frequency=\"24h\"\n", name, name+"-bucket")
	}

	cfg := fmt.Sprintf(`
[global]
secrets_command = "true"
cache_dir = %q

[[credentials]]
name = "cred-a"
endpoint = "https://fsn1.example.com"
region = "fsn1"
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
	code := run(context.Background(), []string{"status", "--config", cfgPath}, &out, &errBuf)
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
			states:   map[string]model.RepoState{}, // config will have no repos; add one below
			wantCode: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := tt.states
			cfgPath := setup(t, states)
			if tt.name == "missing cache (grey) yields 2" {
				// Rewrite config with a repo that has no cache entry.
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
	defer f.Close()
	fmt.Fprint(f, "\n[[repos]]\nname=\"cold-repo\"\ncredential=\"cred-a\"\nbucket=\"cold-bucket\"\nexpected_frequency=\"24h\"\n")
}

func TestStatusOutputFormat(t *testing.T) {
	now := time.Now()
	cfgPath := setup(t, map[string]model.RepoState{
		"homeserver-system": {
			RefreshedAt:   now,
			LastSnapshot:  now.Add(-8 * time.Hour),
			TotalSize:     442000000000,
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
	if !strings.Contains(out, "412 GiB") {
		t.Errorf("expected humanized size, got %q", out)
	}
	if !strings.Contains(out, "240 snaps") {
		t.Errorf("expected snapshot count, got %q", out)
	}
	if !strings.Contains(out, "8h ago") {
		t.Errorf("expected relative time, got %q", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Errorf("expected error message, got %q", errBuf.String())
	}
}

func TestHelpShowsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &out, &errBuf); code != 0 {
		t.Errorf("help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage, got %q", out.String())
	}
}

// The TUI wires secrets and restic before starting Bubble Tea (plan §12). When
// secrets_command cannot produce usable secrets, cmdTUI must fail fast with exit
// 2 rather than launching a screen that can never refresh — and crucially
// without trying to open a terminal in the test harness. The setup config's
// secrets_command ("true") yields no secrets JSON, so refreshDeps fails.
func TestTUIFailsFastWhenSecretsUnavailable(t *testing.T) {
	cfgPath := setup(t, map[string]model.RepoState{
		"repo-a": {RefreshedAt: time.Now(), LastSnapshot: time.Now()},
	})
	var out, errBuf bytes.Buffer
	code := run(context.Background(), []string{"tui", "--config", cfgPath}, &out, &errBuf)
	if code != 2 {
		t.Errorf("tui exit = %d, want 2 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "startup failed") {
		t.Errorf("expected startup failure message, got %q", errBuf.String())
	}
}

// A refresh whose repos are all healthy but whose results could not be
// persisted must still exit 2 ("...or a failure"), not 0 — otherwise cron
// callers miss that the cache is now stale. The end-to-end --refresh path needs
// a real restic and a save-erroring cache, so the policy is tested directly.
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
