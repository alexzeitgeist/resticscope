package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

func TestSaveThenLoad(t *testing.T) {
	s := New(t.TempDir())
	ctx := t.Context()
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
	_, err := s.Load(t.Context(), "nope")
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
	_, err := s.Load(t.Context(), "broken")
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
	ctx := t.Context()
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
	if err := s.Save(t.Context(), "repo", model.RepoState{Name: "repo"}); err != nil {
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
	if err := s.Save(t.Context(), "repo", model.RepoState{Name: "repo"}); err != nil {
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
	// A pre-cleanup cache file still carries fields RepoState/Snapshot no longer
	// represent (total_size, pack_count, partial_err, the top-level paths list,
	// and a nested future_paths list). The decoder must ignore them and still
	// load the supported fields rather than failing. Note: snapshot "paths" is
	// now a supported field on Snapshot, so the placeholder unknown nested key
	// is future_paths instead.
	json := `{
		"name":"repo","status":"green",
		"total_size":442000000000,"pack_count":31204,"partial_err":"stats timed out",
		"paths":["/etc","/var/lib"],
		"snapshots":[{"short_id":"s1","hostname":"homeserver","future_paths":["/etc"]}],
		"future_field":{"nested":true},"extra":42
	}`
	if err := os.WriteFile(filepath.Join(dir, "repo.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(t.Context(), "repo")
	if err != nil {
		t.Fatalf("Load with unknown fields: %v", err)
	}
	if got.Name != "repo" || got.Status != model.StatusGreen {
		t.Errorf("unexpected state: %+v", got)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].ShortID != "s1" {
		t.Errorf("snapshots not loaded past the ignored nested fields: %+v", got.Snapshots)
	}
}

// TestSnapshotInfoFieldsRoundTrip locks the cache-schema additions backing the
// snapshot-info modal: paths, excludes, parent, tree, uid (incl. root 0), gid,
// and the new summary counters must all survive a Save/Load round-trip.
func TestSnapshotInfoFieldsRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	ctx := t.Context()
	uid, gid := uint32(0), uint32(0) // root, which must survive distinct from "absent"
	want := model.RepoState{
		Name:        "repo-info",
		RefreshedAt: time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC),
		Status:      model.StatusGreen,
		Snapshots: []model.Snapshot{{
			ID: "id-full", ShortID: "sf",
			Time:     time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC),
			Hostname: "homeserver", Username: "root",
			Parent:   "parent-id",
			Tree:     "tree-id",
			Paths:    []string{"/etc", "/var/lib"},
			Excludes: []string{"*.tmp", "/var/cache"},
			UID:      &uid,
			GID:      &gid,
			Summary: &model.SnapshotSummary{
				TotalBytesProcessed: 4404019200,
				FilesNew:            uint64Ptr(12),
				FilesChanged:        uint64Ptr(34),
				FilesUnmodified:     uint64Ptr(4050),
				DirsNew:             uint64Ptr(1),
				DirsChanged:         uint64Ptr(2),
				DirsUnmodified:      uint64Ptr(7),
				DataBlobs:           int64Ptr(11),
				TreeBlobs:           int64Ptr(3),
			},
		}},
	}
	if err := s.Save(ctx, want.Name, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, want.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(got.Snapshots))
	}
	gs := got.Snapshots[0]
	ws := want.Snapshots[0]
	if gs.Parent != ws.Parent || gs.Tree != ws.Tree {
		t.Errorf("parent/tree mismatch: %q/%q want %q/%q", gs.Parent, gs.Tree, ws.Parent, ws.Tree)
	}
	if !equalStrings(gs.Paths, ws.Paths) {
		t.Errorf("Paths = %v, want %v", gs.Paths, ws.Paths)
	}
	if !equalStrings(gs.Excludes, ws.Excludes) {
		t.Errorf("Excludes = %v, want %v", gs.Excludes, ws.Excludes)
	}
	if gs.UID == nil || *gs.UID != 0 {
		t.Errorf("UID = %v, want 0 (root preserved as present-and-zero)", gs.UID)
	}
	if gs.GID == nil || *gs.GID != 0 {
		t.Errorf("GID = %v, want 0", gs.GID)
	}
	if gs.Summary == nil {
		t.Fatal("Summary lost on round-trip")
	}
	if gs.Summary.FilesUnmodified == nil || *gs.Summary.FilesUnmodified != 4050 {
		t.Errorf("FilesUnmodified = %v, want 4050", gs.Summary.FilesUnmodified)
	}
	if gs.Summary.DirsNew == nil || gs.Summary.DirsChanged == nil || gs.Summary.DirsUnmodified == nil {
		t.Errorf("dirs counters lost: new=%v changed=%v unmodified=%v",
			gs.Summary.DirsNew, gs.Summary.DirsChanged, gs.Summary.DirsUnmodified)
	}
	if gs.Summary.DataBlobs == nil || *gs.Summary.DataBlobs != 11 {
		t.Errorf("DataBlobs = %v, want 11", gs.Summary.DataBlobs)
	}
	if gs.Summary.TreeBlobs == nil || *gs.Summary.TreeBlobs != 3 {
		t.Errorf("TreeBlobs = %v, want 3", gs.Summary.TreeBlobs)
	}
}

func uint64Ptr(n uint64) *uint64 { return &n }
func int64Ptr(n int64) *int64    { return &n }
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContextCancellation(t *testing.T) {
	s := New(t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Save(ctx, "repo", model.RepoState{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Save with cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := s.Load(ctx, "repo"); !errors.Is(err, context.Canceled) {
		t.Errorf("Load with cancelled ctx = %v, want context.Canceled", err)
	}
}
