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
			tagSet[tg] = struct{}{}
		}
	}
	return sortedKeys(hostSet), sortedKeys(tagSet)
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
