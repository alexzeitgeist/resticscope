package model

import (
	"maps"
	"slices"
	"sort"
	"time"
)

// Observed returns sorted, unique, non-empty hosts and tags. Each result is nil
// when no values were observed.
func Observed(snaps []Snapshot) (hosts, tags []string) {
	hostSet := map[string]struct{}{}
	tagSet := map[string]struct{}{}
	for _, s := range snaps {
		if s.Hostname != "" {
			hostSet[s.Hostname] = struct{}{}
		}
		for _, tg := range s.Tags {
			if tg != "" {
				tagSet[tg] = struct{}{}
			}
		}
	}
	return sortedKeys(hostSet), sortedKeys(tagSet)
}

// ObservedVersions returns sorted, unique, non-empty restic versions. It
// returns nil when none were observed.
func ObservedVersions(snaps []Snapshot) []string {
	verSet := map[string]struct{}{}
	for _, s := range snaps {
		if s.ProgramVersion != "" {
			verSet[s.ProgramVersion] = struct{}{}
		}
	}
	return sortedKeys(verSet)
}

// LastBackupDuration returns the newest snapshot's non-negative backup duration.
// The result is unavailable when the snapshot or complete timestamps are absent.
func LastBackupDuration(snaps []Snapshot) (time.Duration, bool) {
	newest, ok := latestSnapshot(snaps)
	if !ok {
		return 0, false
	}
	return SnapshotBackupDuration(newest)
}

// SnapshotBackupDuration returns a snapshot's non-negative backup duration.
// The result is unavailable when its summary or complete timestamps are absent.
func SnapshotBackupDuration(s Snapshot) (time.Duration, bool) {
	if s.Summary == nil || s.Summary.BackupStart.IsZero() || s.Summary.BackupEnd.IsZero() {
		return 0, false
	}
	d := s.Summary.BackupEnd.Sub(s.Summary.BackupStart)
	if d < 0 {
		return 0, false
	}
	return d, true
}

func latestSnapshot(snaps []Snapshot) (Snapshot, bool) {
	var newest Snapshot
	var ok bool
	for _, s := range snaps {
		if !ok || snapshotBeforeNewest(s, newest) {
			newest = s
			ok = true
		}
	}
	return newest, ok
}

// SortedSnapshotsNewestFirst returns a newest-first copy using the package's
// deterministic snapshot tie-breakers.
func SortedSnapshotsNewestFirst(src []Snapshot) []Snapshot {
	snaps := make([]Snapshot, len(src))
	copy(snaps, src)
	SortSnapshotsNewestFirst(snaps)
	return snaps
}

// SortSnapshotsNewestFirst orders snapshots in place by descending time, ID,
// then ShortID.
func SortSnapshotsNewestFirst(snaps []Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool { return snapshotBeforeNewest(snaps[i], snaps[j]) })
}

func snapshotBeforeNewest(a, b Snapshot) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	if a.ID != b.ID {
		return a.ID > b.ID
	}
	return a.ShortID > b.ShortID
}

// LatestSnapshotTime returns the most recent snapshot time, or zero when empty.
func LatestSnapshotTime(snaps []Snapshot) time.Time {
	var latest time.Time
	for _, s := range snaps {
		if s.Time.After(latest) {
			latest = s.Time
		}
	}
	return latest
}

func sortedKeys(set map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(set))
}
