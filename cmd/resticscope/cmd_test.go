package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestNoArgsShowsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), nil, &out, &errBuf); code != 2 {
		t.Errorf("no-args exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "Usage:") {
		t.Errorf("expected usage, got %q", errBuf.String())
	}
}

func TestHumanizeBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{442000000000, "412 GiB"},
		{4400000000, "4.1 GiB"},
		{18000000000, "17 GiB"},
		{72 * 1024 * 1024 * 1024, "72 GiB"},
	}
	for _, tt := range tests {
		if got := humanizeBytes(tt.n); got != tt.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestHumanizeAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "never"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-35 * time.Minute), "35m ago"},
		{now.Add(-8 * time.Hour), "8h ago"},
		{now.Add(-9 * 24 * time.Hour), "9d ago"},
	}
	for _, tt := range tests {
		if got := humanizeAgo(now, tt.t); got != tt.want {
			t.Errorf("humanizeAgo(%v) = %q, want %q", tt.t, got, tt.want)
		}
	}
}
