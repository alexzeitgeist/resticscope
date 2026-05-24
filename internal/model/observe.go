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
// BackupStart of the newest snapshot. It scans by snapshot time (not slice
// order; exact timestamp ties use snapshot IDs for stable output) and returns 0
// when the newest snapshot has no summary (pre-0.17 snapshots) or incomplete
// timestamps — the "unknown" value humanize.Duration renders as an em-dash.
func LastBackupDuration(snaps []Snapshot) time.Duration {
	newest, ok := latestSnapshot(snaps)
	if !ok || newest.Summary == nil || newest.Summary.BackupStart.IsZero() || newest.Summary.BackupEnd.IsZero() {
		return 0
	}
	return newest.Summary.BackupEnd.Sub(newest.Summary.BackupStart)
}

func latestSnapshot(snaps []Snapshot) (Snapshot, bool) {
	var newest Snapshot
	var ok bool
	for _, s := range snaps {
		if !ok || snapshotAfter(s, newest) {
			newest = s
			ok = true
		}
	}
	return newest, ok
}

func snapshotAfter(a, b Snapshot) bool {
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
