package model

import "time"

// RepoState is the cached, observed state of a single repository. It is the
// only resticscope-authored structure persisted to disk (one JSON file per
// repo), so it must never hold credentials.
//
// The schema is forward-compatible: decoders ignore unknown fields, so new
// fields can be added without invalidating existing cache files.
type RepoState struct {
	Name          string     `json:"name"`
	RefreshedAt   time.Time  `json:"refreshed_at"`
	Status        Status     `json:"status"`
	SnapshotCount int        `json:"snapshot_count"`
	LastSnapshot  time.Time  `json:"last_snapshot"`
	LockedSince   *time.Time `json:"locked_since,omitempty"`
	Snapshots     []Snapshot `json:"snapshots,omitempty"`
	Hosts         []string   `json:"hosts,omitempty"` // observed, derived from snapshots
	Tags          []string   `json:"tags,omitempty"`  // observed
	LastError     string     `json:"last_error,omitempty"`
	ResticVer     string     `json:"restic_version,omitempty"`
}

// Refreshed reports whether this state has ever been successfully refreshed.
func (s RepoState) Refreshed() bool {
	return !s.RefreshedAt.IsZero()
}
