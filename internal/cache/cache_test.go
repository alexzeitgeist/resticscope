package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"resticscope/internal/model"
)

func TestSaveThenLoad(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	want := model.RepoState{
		Name:          "homeserver-system",
		RefreshedAt:   time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC),
		Status:        model.StatusGreen,
		SnapshotCount: 240,
		LastSnapshot:  time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC),
		Hosts:         []string{"homeserver"},
	}
	if err := s.Save(ctx, want.Name, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, want.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != want.Name || got.Status != want.Status || got.SnapshotCount != want.SnapshotCount {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if !got.RefreshedAt.Equal(want.RefreshedAt) {
		t.Errorf("RefreshedAt mismatch: got %v want %v", got.RefreshedAt, want.RefreshedAt)
	}
}

func TestLoadMissingIsMiss(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.Load(context.Background(), "nope")
	if !errors.Is(err, ErrMiss) {
		t.Fatalf("expected ErrMiss, got %v", err)
	}
}

func TestLoadCorruptIsMiss(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load(context.Background(), "broken")
	if !errors.Is(err, ErrMiss) {
		t.Errorf("corrupt file should be a miss, got %v", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt file should be reported as corrupt, got %v", err)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()
	if err := s.Save(ctx, "repo", model.RepoState{Name: "repo"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// No leftover temp files should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "repo.json" {
		t.Errorf("unexpected cache dir contents: %v", entries)
	}
}

func TestSaveFilePerms(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(context.Background(), "repo", model.RepoState{Name: "repo"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache file perms = %o, want 600", perm)
	}
}

func TestNoCredentialKeysInCacheFile(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(context.Background(), "repo", model.RepoState{Name: "repo"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access_key", "secret_key", "restic_password", "password"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("cache file contains forbidden key %q", forbidden)
		}
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// A pre-cleanup cache file still carries fields that RepoState/Snapshot no
	// longer represent (total_size, pack_count, partial_err, the top-level paths
	// list, and a nested snapshot paths list). The decoder must ignore them and
	// still load the supported fields rather than failing.
	json := `{
		"name":"repo","status":"green",
		"total_size":442000000000,"pack_count":31204,"partial_err":"stats timed out",
		"paths":["/etc","/var/lib"],
		"snapshots":[{"short_id":"s1","hostname":"homeserver","paths":["/etc"]}],
		"future_field":{"nested":true},"extra":42
	}`
	if err := os.WriteFile(filepath.Join(dir, "repo.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(context.Background(), "repo")
	if err != nil {
		t.Fatalf("Load with unknown fields: %v", err)
	}
	if got.Name != "repo" || got.Status != model.StatusGreen {
		t.Errorf("unexpected state: %+v", got)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].ShortID != "s1" {
		t.Errorf("snapshots not loaded past the ignored nested paths: %+v", got.Snapshots)
	}
}

func TestContextCancellation(t *testing.T) {
	s := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Save(ctx, "repo", model.RepoState{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Save with cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := s.Load(ctx, "repo"); !errors.Is(err, context.Canceled) {
		t.Errorf("Load with cancelled ctx = %v, want context.Canceled", err)
	}
}
