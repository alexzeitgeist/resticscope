package model

import (
	"reflect"
	"testing"
	"time"
)

// mustTime panics when a test timestamp is malformed.
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func snapshotsByID(snaps ...Snapshot) map[string]Snapshot {
	m := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		m[s.ID] = s
	}
	return m
}

func TestGroupFileVersions(t *testing.T) {
	path := "/etc/passwd"
	mtA := mustTime("2026-05-01T00:00:00Z")
	mtB := mustTime("2026-05-15T00:00:00Z")

	snapOld := Snapshot{ID: "snap-1", ShortID: "snap0001", Time: mustTime("2026-05-02T06:00:00Z"), Hostname: "home"}
	snapMid := Snapshot{ID: "snap-2", ShortID: "snap0002", Time: mustTime("2026-05-16T06:00:00Z"), Hostname: "home"}
	snapNew := Snapshot{ID: "snap-3", ShortID: "snap0003", Time: mustTime("2026-05-23T06:00:00Z"), Hostname: "home"}

	mkMatch := func(size int64, mt time.Time) FindMatch {
		return FindMatch{Path: path, Name: "passwd", Type: "file", Size: size, ModTime: mt, Permissions: "-rw-r--r--"}
	}

	t.Run("same key collapses to one row, two occurrences", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, mtA)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1500, mtA)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid))
		if len(got) != 1 || len(got[0].Occurrences) != 2 {
			t.Fatalf("want one row with two occurrences, got %+v", got)
		}
		if got[0].Occurrences[0].SnapshotID != "snap-2" || got[0].Occurrences[1].SnapshotID != "snap-1" {
			t.Errorf("occurrences not newest-first: %+v", got[0].Occurrences)
		}
	})

	t.Run("same instant in different locations collapses", func(t *testing.T) {
		berlin := time.FixedZone("CET", 3600)
		sameInstant := mtA.In(berlin)
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, mtA)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1500, sameInstant)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid))
		if len(got) != 1 || len(got[0].Occurrences) != 2 {
			t.Fatalf("want one row for equal instants across locations, got %+v", got)
		}
	})

	t.Run("out-of-range mtimes stay distinct", func(t *testing.T) {
		// A UnixNano key can wrap this pre-1678 instant onto a distinct
		// in-range timestamp; normalized time.Time keys cannot.
		preUnixNanoRange := time.Unix(mtA.Unix()-18446744073, int64(mtA.Nanosecond())-709551616).UTC()
		if !preUnixNanoRange.Before(time.Date(1678, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("test setup expected pre-1678 time, got %v", preUnixNanoRange)
		}
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, preUnixNanoRange)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1500, mtA)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid))
		if len(got) != 2 {
			t.Fatalf("want distinct rows for mtimes outside UnixNano's defined range, got %+v", got)
		}
	})

	t.Run("size differs: two rows", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, mtA)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1501, mtA)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid))
		if len(got) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
		}
	})

	t.Run("mtime differs: two rows", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, mtA)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1500, mtB)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid))
		if len(got) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
		}
	})

	t.Run("A then B then A non-adjacent rollback collapses A occurrences", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-3", Matches: []FindMatch{mkMatch(1500, mtA)}},
			{SnapshotID: "snap-2", Matches: []FindMatch{mkMatch(1500, mtB)}},
			{SnapshotID: "snap-1", Matches: []FindMatch{mkMatch(1500, mtA)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld, snapMid, snapNew))
		if len(got) != 2 {
			t.Fatalf("want 2 rows (A and B), got %d: %+v", len(got), got)
		}
		var aRow, bRow *FileVersion
		for i := range got {
			if got[i].ModTime.Equal(mtA) {
				aRow = &got[i]
			} else if got[i].ModTime.Equal(mtB) {
				bRow = &got[i]
			}
		}
		if aRow == nil || bRow == nil {
			t.Fatalf("missing A or B group: %+v", got)
		}
		if len(aRow.Occurrences) != 2 {
			t.Fatalf("A group should have 2 occurrences (snap-1 and snap-3), got %d", len(aRow.Occurrences))
		}
		if aRow.Occurrences[0].SnapshotID != "snap-3" || aRow.Occurrences[1].SnapshotID != "snap-1" {
			t.Errorf("A occurrences not newest-first: %+v", aRow.Occurrences)
		}
		if !got[0].ModTime.Equal(mtA) {
			t.Errorf("expected A row first (newest occurrence newer than B), got %+v", got)
		}
	})

	t.Run("unknown snapshot id yields empty occurrence fields", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-ghost", Matches: []FindMatch{mkMatch(1500, mtA)}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld))
		if len(got) != 1 || len(got[0].Occurrences) != 1 {
			t.Fatalf("want one row, one occurrence, got %+v", got)
		}
		occ := got[0].Occurrences[0]
		if occ.SnapshotID != "snap-ghost" {
			t.Errorf("snapshot id lost: %+v", occ)
		}
		if occ.ShortID != "" || !occ.SnapTime.IsZero() || occ.Hostname != "" {
			t.Errorf("expected empty short/time/host for unknown snapshot, got %+v", occ)
		}
	})

	t.Run("empty input is nil", func(t *testing.T) {
		if got := GroupFileVersions(nil, path, nil); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
		if got := GroupFileVersions([]FindSnapshotResult{}, path, nil); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("bracket-glob foreign match dropped", func(t *testing.T) {
		askPath := "/data/a[1].txt"
		foreign := "/data/a1.txt"
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: askPath, Size: 100, ModTime: mtA, Permissions: "-rw-r--r--"},
			}},
			{SnapshotID: "snap-2", Matches: []FindMatch{
				{Path: foreign, Size: 100, ModTime: mtA, Permissions: "-rw-r--r--"},
			}},
		}
		got := GroupFileVersions(results, askPath, snapshotsByID(snapOld, snapMid))
		if len(got) != 1 {
			t.Fatalf("want one row (foreign dropped), got %d: %+v", len(got), got)
		}
		if len(got[0].Occurrences) != 1 || got[0].Occurrences[0].SnapshotID != "snap-1" {
			t.Errorf("expected only snap-1 in occurrences, got %+v", got[0].Occurrences)
		}
	})

	t.Run("star-glob foreign match dropped", func(t *testing.T) {
		askPath := "/data/star*.txt"
		foreign := "/data/starXYZ.txt"
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: askPath, Size: 100, ModTime: mtA},
			}},
			{SnapshotID: "snap-2", Matches: []FindMatch{
				{Path: foreign, Size: 100, ModTime: mtA},
			}},
		}
		got := GroupFileVersions(results, askPath, snapshotsByID(snapOld, snapMid))
		if len(got) != 1 || len(got[0].Occurrences) != 1 {
			t.Fatalf("expected one row, one occurrence; got %+v", got)
		}
		if got[0].Occurrences[0].SnapshotID != "snap-1" {
			t.Errorf("foreign match was not dropped: %+v", got[0].Occurrences)
		}
	})

	t.Run("mixed real and foreign matches inside one snapshot", func(t *testing.T) {
		askPath := "/data/file.txt"
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: askPath, Size: 100, ModTime: mtA},
				{Path: "/data/other.txt", Size: 100, ModTime: mtA},
			}},
			{SnapshotID: "snap-2", Matches: []FindMatch{
				{Path: "/data/other.txt", Size: 100, ModTime: mtA},
			}},
		}
		got := GroupFileVersions(results, askPath, snapshotsByID(snapOld, snapMid))
		if len(got) != 1 || len(got[0].Occurrences) != 1 {
			t.Fatalf("want one row with one occurrence (snap-1), got %+v", got)
		}
		if got[0].Occurrences[0].SnapshotID != "snap-1" {
			t.Errorf("expected snap-1 occurrence, got %+v", got[0].Occurrences)
		}
	})

	t.Run("uid/gid plumbed through with OwnerKnown true", func(t *testing.T) {
		uid, gid := uint32(0), uint32(0)
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: path, Size: 1500, ModTime: mtA, Permissions: "-rw-r--r--", UID: &uid, GID: &gid},
			}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld))
		if len(got) != 1 {
			t.Fatalf("want one row, got %+v", got)
		}
		if !got[0].OwnerKnown || got[0].UID != 0 || got[0].GID != 0 {
			t.Errorf("expected OwnerKnown=true uid=0 gid=0, got %+v", got[0])
		}
	})

	t.Run("missing uid/gid leaves OwnerKnown false", func(t *testing.T) {
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: path, Size: 1500, ModTime: mtA, Permissions: "-rw-r--r--"},
			}},
		}
		got := GroupFileVersions(results, path, snapshotsByID(snapOld))
		if len(got) != 1 {
			t.Fatalf("want one row, got %+v", got)
		}
		if got[0].OwnerKnown {
			t.Errorf("expected OwnerKnown=false when match carried no uid/gid, got %+v", got[0])
		}
	})

	t.Run("all-foreign result yields nil", func(t *testing.T) {
		askPath := "/data/file.txt"
		results := []FindSnapshotResult{
			{SnapshotID: "snap-1", Matches: []FindMatch{
				{Path: "/data/other.txt", Size: 100, ModTime: mtA},
			}},
		}
		got := GroupFileVersions(results, askPath, snapshotsByID(snapOld))
		if got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})
}

func TestGroupFileVersionsShape(t *testing.T) {
	path := "/etc/hostname"
	mt := mustTime("2026-05-01T00:00:00Z")
	snap := Snapshot{ID: "s1", ShortID: "s1short1", Time: mustTime("2026-05-10T00:00:00Z"), Hostname: "h1"}
	results := []FindSnapshotResult{
		{SnapshotID: "s1", Matches: []FindMatch{
			{Path: path, Size: 7, ModTime: mt, Permissions: "-rw-r--r--"},
		}},
	}
	want := []FileVersion{{
		Size:        7,
		ModTime:     mt,
		Permissions: "-rw-r--r--",
		Occurrences: []FileVersionOccurrence{
			{SnapshotID: "s1", ShortID: "s1short1", SnapTime: snap.Time, Hostname: "h1"},
		},
	}}
	got := GroupFileVersions(results, path, snapshotsByID(snap))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shape mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

// Typed non-file matches cannot support extraction's regular-file attestation.
// Empty types remain accepted but cannot attest a regular-file source.
func TestGroupFileVersionsSkipsNonFileMatches(t *testing.T) {
	mt := mustTime("2026-05-01T10:00:00Z")
	results := []FindSnapshotResult{
		{SnapshotID: "s1", Matches: []FindMatch{{Path: "/f", Type: "file", Size: 5, ModTime: mt}}},
		{SnapshotID: "s2", Matches: []FindMatch{{Path: "/f", Type: "symlink", Size: 9, ModTime: mt}}},
		{SnapshotID: "s3", Matches: []FindMatch{{Path: "/f", Type: "socket", Size: 5, ModTime: mt}}},
		{SnapshotID: "s4", Matches: []FindMatch{{Path: "/f", Size: 5, ModTime: mt}}},
	}
	got := GroupFileVersions(results, "/f", nil)
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1 (non-file matches must not form versions)", len(got))
	}
	if len(got[0].Occurrences) != 2 {
		t.Fatalf("occurrences = %d, want 2 (typed file s1 + type-less s4)", len(got[0].Occurrences))
	}
	for _, o := range got[0].Occurrences {
		if o.SnapshotID == "s2" || o.SnapshotID == "s3" {
			t.Errorf("non-file occurrence %s survived grouping", o.SnapshotID)
		}
	}
}
