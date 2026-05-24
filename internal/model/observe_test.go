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
		{ProgramVersion: ""}, // empty must be dropped
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
		mk(21, d1s, d1s.Add(time.Minute)),    // older, has summary
		mk(23, d3s, d3s.Add(28*time.Second)), // newest with summary → wins
		mk(22, time.Time{}, time.Time{}),     // no summary, must be skipped
	}
	if got := LastBackupDuration(snaps); got != 28*time.Second {
		t.Errorf("duration = %v, want 28s", got)
	}

	latestUnsummarized := []Snapshot{
		mk(21, d1s, d1s.Add(time.Minute)),
		mk(23, time.Time{}, time.Time{}),
	}
	if got := LastBackupDuration(latestUnsummarized); got != 0 {
		t.Errorf("duration = %v, want 0 when the latest snapshot has no summary", got)
	}

	tieA := mk(23, d3s, d3s.Add(10*time.Second))
	tieA.ID = "a"
	tieB := mk(23, d3s, d3s.Add(20*time.Second))
	tieB.ID = "b"
	for _, snaps := range [][]Snapshot{{tieA, tieB}, {tieB, tieA}} {
		if got := LastBackupDuration(snaps); got != 20*time.Second {
			t.Errorf("duration for tied snapshot times = %v, want stable ID tie-break duration 20s", got)
		}
	}

	// No snapshot carries a summary → unknown (0).
	none := []Snapshot{mk(21, time.Time{}, time.Time{}), mk(22, time.Time{}, time.Time{})}
	if got := LastBackupDuration(none); got != 0 {
		t.Errorf("duration = %v, want 0 when no summaries", got)
	}
	if got := LastBackupDuration(nil); got != 0 {
		t.Errorf("empty duration = %v, want 0", got)
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
