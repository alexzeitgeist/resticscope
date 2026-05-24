package model

import (
	"sort"
	"time"
)

// Observed extracts the de-duplicated, sorted hosts and tags seen across the
// given snapshots. They are surfaced as informational detail-view lines. Each
// return value is nil when nothing was seen, keeping JSON output and equality
// checks clean.
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

// ObservedVersions returns the de-duplicated, sorted restic program versions
// seen across the given snapshots, so a detail view can reveal a fleet running
// mixed or outdated clients. It mirrors Observed's nil-on-empty convention.
func ObservedVersions(snaps []Snapshot) []string {
	verSet := map[string]struct{}{}
	for _, s := range snaps {
		if s.ProgramVersion != "" {
			verSet[s.ProgramVersion] = struct{}{}
		}
	}
	return sortedKeys(verSet)
}

// LastBackupDuration returns how long the most recent backup took: BackupEnd-
// BackupStart of the newest snapshot. The bool is false when there is no newest
// snapshot, or when the newest snapshot has no complete, non-negative duration.
func LastBackupDuration(snaps []Snapshot) (time.Duration, bool) {
	newest, ok := latestSnapshot(snaps)
	if !ok {
		return 0, false
	}
	return SnapshotBackupDuration(newest)
}

// SnapshotBackupDuration returns the duration recorded on a snapshot summary.
// The bool is false when the snapshot has no summary, incomplete timestamps, or
// an invalid negative duration.
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

// SortedSnapshotsNewestFirst copies snapshots and orders them newest-first using
// the same tie-breaks as LastBackupDuration.
func SortedSnapshotsNewestFirst(src []Snapshot) []Snapshot {
	snaps := make([]Snapshot, len(src))
	copy(snaps, src)
	SortSnapshotsNewestFirst(snaps)
	return snaps
}

// SortSnapshotsNewestFirst orders snapshots newest-first in place. Exact
// timestamp ties are broken by ID, then ShortID, so renderers do not fall back to
// input slice order.
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

// LatestSnapshotTime returns the most recent snapshot time, or the zero time
// when there are no snapshots.
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
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
