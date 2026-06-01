package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"resticscope/internal/model"
)

func findTestApp(fc *fakeCache, fr fakeRestic) *App {
	return &App{
		Cfg:     testConfig(),
		Cache:   fc,
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fr,
	}
}

// seedSnapshot writes a single snapshot into the fake cache so the grouping step
// can join snapshot metadata without needing a real refresh.
func seedSnapshot(fc *fakeCache, repoName, snapID, host string, ts time.Time) {
	fc.states[repoName] = model.RepoState{
		Name:        repoName,
		RefreshedAt: now,
		Snapshots: []model.Snapshot{
			{ID: snapID, ShortID: snapID[:8], Time: ts, Hostname: host},
		},
		SnapshotCount: 1,
		LastSnapshot:  ts,
	}
}

func TestFindFileVersionsHostFilter(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	snapID := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
	fc := newFakeCache()
	seedSnapshot(fc, "repo-a", snapID, "homeserver", now.Add(-2*time.Hour))

	cap := &findCapture{}
	results := []model.FindSnapshotResult{
		{SnapshotID: snapID, Hits: 1, Matches: []model.FindMatch{
			{Path: "/etc/hostname", Size: 12, ModTime: mt, Permissions: "-rw-r--r--"},
		}},
	}
	a := findTestApp(fc, fakeRestic{findResults: results, findCap: cap})

	got, err := a.FindFileVersions(context.Background(), "repo-a", "homeserver", "/etc/hostname", false)
	if err != nil {
		t.Fatalf("FindFileVersions: %v", err)
	}
	if cap.calls != 1 {
		t.Fatalf("FindMatches called %d times, want exactly 1", cap.calls)
	}
	if cap.host != "homeserver" {
		t.Errorf("host = %q, want homeserver", cap.host)
	}
	if cap.pattern != "/etc/hostname" {
		t.Errorf("pattern = %q, want /etc/hostname", cap.pattern)
	}
	if got.Host != "homeserver" || got.AllHosts {
		t.Errorf("result = %+v, want Host=homeserver AllHosts=false", got)
	}
	if len(got.Rows) != 1 || got.Rows[0].Size != 12 {
		t.Errorf("rows = %+v, want one row with Size=12", got.Rows)
	}
}

func TestFindFileVersionsAllHosts(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	snapID := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
	fc := newFakeCache()
	seedSnapshot(fc, "repo-a", snapID, "homeserver", now.Add(-2*time.Hour))

	cap := &findCapture{}
	results := []model.FindSnapshotResult{
		{SnapshotID: snapID, Hits: 1, Matches: []model.FindMatch{
			{Path: "/etc/hostname", Size: 12, ModTime: mt},
		}},
	}
	a := findTestApp(fc, fakeRestic{findResults: results, findCap: cap})

	got, err := a.FindFileVersions(context.Background(), "repo-a", "homeserver", "/etc/hostname", true)
	if err != nil {
		t.Fatalf("FindFileVersions: %v", err)
	}
	if cap.host != "" {
		t.Errorf("host = %q, want empty for all-hosts", cap.host)
	}
	if got.Host != "" || !got.AllHosts {
		t.Errorf("result = %+v, want Host=\"\" AllHosts=true", got)
	}
}

func TestFindFileVersionsErrFindUnknownHost(t *testing.T) {
	// No origin host supplied — the app promotes that to ErrFindUnknownHost when
	// allHosts is false; no FindMatches call must happen, since silently widening
	// to all hosts would defeat the host filter.
	fc := newFakeCache()
	cap := &findCapture{}
	a := findTestApp(fc, fakeRestic{findCap: cap})

	_, err := a.FindFileVersions(context.Background(), "repo-a", "", "/etc/hostname", false)
	if !errors.Is(err, ErrFindUnknownHost) {
		t.Fatalf("expected ErrFindUnknownHost, got %v", err)
	}
	if cap.calls != 0 {
		t.Errorf("FindMatches must not be called when host is unknown, got calls=%d", cap.calls)
	}
}

func TestFindFileVersionsUsesLiveOriginHostWhenCacheMisses(t *testing.T) {
	// A successful in-session refresh can update the TUI's live rows even if the
	// cache write fails. FindFileVersions must use the selected snapshot's live
	// host passed by the caller, not rediscover the host from the persisted cache.
	fc := newFakeCache()
	snapID := "snap-live-only"
	cap := &findCapture{}
	results := []model.FindSnapshotResult{
		{SnapshotID: snapID, Hits: 1, Matches: []model.FindMatch{
			{Path: "/etc/hostname", Size: 12, ModTime: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
		}},
	}
	a := findTestApp(fc, fakeRestic{findResults: results, findCap: cap})

	got, err := a.FindFileVersions(context.Background(), "repo-a", "live-host", "/etc/hostname", false)
	if err != nil {
		t.Fatalf("FindFileVersions: %v", err)
	}
	if cap.host != "live-host" {
		t.Errorf("host = %q, want live-host from caller", cap.host)
	}
	if got.Host != "live-host" || got.AllHosts {
		t.Errorf("result = %+v, want Host=live-host AllHosts=false", got)
	}
}

func TestFindFileVersionsUnknownHostRecoverableViaAllHosts(t *testing.T) {
	// When the originating host is unknown but the user opts into all-hosts,
	// the call must succeed without --host. This is the recovery affordance.
	fc := newFakeCache()
	cap := &findCapture{}
	a := findTestApp(fc, fakeRestic{findCap: cap})

	_, err := a.FindFileVersions(context.Background(), "repo-a", "", "/etc/hostname", true)
	if err != nil {
		t.Fatalf("FindFileVersions (all-hosts recovery): %v", err)
	}
	if cap.host != "" {
		t.Errorf("host = %q, want empty under all-hosts recovery", cap.host)
	}
	if cap.calls != 1 {
		t.Errorf("FindMatches calls = %d, want 1", cap.calls)
	}
}

func TestFindFileVersionsUnknownRepo(t *testing.T) {
	fc := newFakeCache()
	a := findTestApp(fc, fakeRestic{})
	if _, err := a.FindFileVersions(context.Background(), "no-such-repo", "host", "/etc", false); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestFindFileVersionsSurfacesResticError(t *testing.T) {
	snapID := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
	fc := newFakeCache()
	seedSnapshot(fc, "repo-a", snapID, "homeserver", now.Add(-time.Hour))
	a := findTestApp(fc, fakeRestic{findErr: errors.New("restic find: boom")})

	if _, err := a.FindFileVersions(context.Background(), "repo-a", "homeserver", "/x", false); err == nil {
		t.Fatal("expected restic error to surface")
	}
}

func TestFindFileVersionsGroupsRowsAcrossSnapshots(t *testing.T) {
	mt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	snapA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	fc := newFakeCache()
	fc.states["repo-a"] = model.RepoState{
		Name: "repo-a", RefreshedAt: now,
		Snapshots: []model.Snapshot{
			{ID: snapA, ShortID: snapA[:8], Time: now.Add(-2 * time.Hour), Hostname: "homeserver"},
			{ID: snapB, ShortID: snapB[:8], Time: now.Add(-time.Hour), Hostname: "homeserver"},
		},
		SnapshotCount: 2,
	}
	results := []model.FindSnapshotResult{
		{SnapshotID: snapA, Hits: 1, Matches: []model.FindMatch{{Path: "/etc/x", Size: 100, ModTime: mt}}},
		{SnapshotID: snapB, Hits: 1, Matches: []model.FindMatch{{Path: "/etc/x", Size: 100, ModTime: mt}}},
	}
	a := findTestApp(fc, fakeRestic{findResults: results})

	got, err := a.FindFileVersions(context.Background(), "repo-a", "homeserver", "/etc/x", false)
	if err != nil {
		t.Fatalf("FindFileVersions: %v", err)
	}
	if len(got.Rows) != 1 || len(got.Rows[0].Occurrences) != 2 {
		t.Fatalf("expected one row with two occurrences, got %+v", got.Rows)
	}
}
