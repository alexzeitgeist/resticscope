package model

import (
	"reflect"
	"testing"
	"time"
)

func TestObserved(t *testing.T) {
	snaps := []Snapshot{
		{Hostname: "homeserver", Tags: []string{"daily", "system"}},
		{Hostname: "homeserver", Tags: []string{"daily"}},
		{Hostname: "laptop", Tags: nil},
	}
	hosts, tags := Observed(snaps)
	if want := []string{"homeserver", "laptop"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	if want := []string{"daily", "system"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestObservedFiltersEmptyTagsAndHosts(t *testing.T) {
	snaps := []Snapshot{
		{Hostname: "", Tags: []string{"", "daily"}},
		{Hostname: "laptop", Tags: []string{""}},
	}
	hosts, tags := Observed(snaps)
	if want := []string{"laptop"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v (empty hostname must be dropped)", hosts, want)
	}
	if want := []string{"daily"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v (empty tag must be dropped)", tags, want)
	}
}

func TestObservedEmpty(t *testing.T) {
	hosts, tags := Observed(nil)
	if hosts != nil || tags != nil {
		t.Errorf("expected nil slices, got %v %v", hosts, tags)
	}
}

func TestObservedVersions(t *testing.T) {
	snaps := []Snapshot{
		{ProgramVersion: "restic 0.18.1"},
		{ProgramVersion: "restic 0.17.0"},
		{ProgramVersion: "restic 0.18.1"},
		{ProgramVersion: ""},
	}
	if want := []string{"restic 0.17.0", "restic 0.18.1"}; !reflect.DeepEqual(ObservedVersions(snaps), want) {
		t.Errorf("versions = %v, want %v", ObservedVersions(snaps), want)
	}
	if v := ObservedVersions(nil); v != nil {
		t.Errorf("empty versions = %v, want nil", v)
	}
}

func TestLastBackupDuration(t *testing.T) {
	mk := func(day int, start, end time.Time) Snapshot {
		s := Snapshot{Time: time.Date(2026, 5, day, 6, 0, 0, 0, time.UTC)}
		if !start.IsZero() {
			s.Summary = &SnapshotSummary{BackupStart: start, BackupEnd: end}
		}
		return s
	}
	d1s := time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC)
	d3s := time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC)

	snaps := []Snapshot{
		mk(21, d1s, d1s.Add(time.Minute)),
		mk(23, d3s, d3s.Add(28*time.Second)),
		mk(22, time.Time{}, time.Time{}),
	}
	if got, ok := LastBackupDuration(snaps); !ok || got != 28*time.Second {
		t.Errorf("duration = %v, %v, want 28s, true", got, ok)
	}

	latestUnsummarized := []Snapshot{
		mk(21, d1s, d1s.Add(time.Minute)),
		mk(23, time.Time{}, time.Time{}),
	}
	if got, ok := LastBackupDuration(latestUnsummarized); ok || got != 0 {
		t.Errorf("duration = %v, %v, want 0, false when the latest snapshot has no summary", got, ok)
	}

	tieA := mk(23, d3s, d3s.Add(10*time.Second))
	tieA.ID = "a"
	tieB := mk(23, d3s, d3s.Add(20*time.Second))
	tieB.ID = "b"
	for _, snaps := range [][]Snapshot{{tieA, tieB}, {tieB, tieA}} {
		if got, ok := LastBackupDuration(snaps); !ok || got != 20*time.Second {
			t.Errorf("duration for tied snapshot times = %v, %v, want stable ID tie-break duration 20s, true", got, ok)
		}
	}

	zero := mk(23, d3s, d3s)
	if got, ok := LastBackupDuration([]Snapshot{zero}); !ok || got != 0 {
		t.Errorf("zero duration = %v, %v, want 0, true", got, ok)
	}

	none := []Snapshot{mk(21, time.Time{}, time.Time{}), mk(22, time.Time{}, time.Time{})}
	if got, ok := LastBackupDuration(none); ok || got != 0 {
		t.Errorf("duration = %v, %v, want 0, false when no summaries", got, ok)
	}
	if got, ok := LastBackupDuration(nil); ok || got != 0 {
		t.Errorf("empty duration = %v, %v, want 0, false", got, ok)
	}
}

func TestSortedSnapshotsNewestFirst(t *testing.T) {
	tm := time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC)
	snaps := []Snapshot{
		{ID: "a", ShortID: "z", Time: tm},
		{ID: "b", ShortID: "a", Time: tm},
		{ID: "c", ShortID: "a", Time: tm.Add(-time.Hour)},
	}

	got := SortedSnapshotsNewestFirst(snaps)
	if got[0].ID != "b" || got[1].ID != "a" || got[2].ID != "c" {
		t.Errorf("sorted IDs = %s, %s, %s; want b, a, c", got[0].ID, got[1].ID, got[2].ID)
	}
	if snaps[0].ID != "a" {
		t.Errorf("SortedSnapshotsNewestFirst mutated input: first ID = %s, want a", snaps[0].ID)
	}
}

func TestLatestSnapshotTime(t *testing.T) {
	t1 := time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 22, 6, 0, 0, 0, time.UTC)
	got := LatestSnapshotTime([]Snapshot{{Time: t1}, {Time: t2}, {Time: t3}})
	if !got.Equal(t2) {
		t.Errorf("latest = %v, want %v", got, t2)
	}
	if z := LatestSnapshotTime(nil); !z.IsZero() {
		t.Errorf("empty latest = %v, want zero", z)
	}
}
