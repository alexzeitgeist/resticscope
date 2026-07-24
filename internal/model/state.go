package model

import "time"

// RepoState is the credential-free observed state persisted for a repository.
// Hosts and Tags are derived from snapshots. Decoders ignore unknown fields for
// forward compatibility.
type RepoState struct {
	Name          string     `json:"name"`
	RefreshedAt   time.Time  `json:"refreshed_at"`
	Status        Status     `json:"status"`
	SnapshotCount int        `json:"snapshot_count"`
	LastSnapshot  time.Time  `json:"last_snapshot"`
	LockedSince   *time.Time `json:"locked_since,omitempty"`
	Snapshots     []Snapshot `json:"snapshots,omitempty"`
	Hosts         []string   `json:"hosts,omitempty"`
	Tags          []string   `json:"tags,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	ResticVer     string     `json:"restic_version,omitempty"`
}

// Refreshed reports whether this state has ever been successfully refreshed.
func (s RepoState) Refreshed() bool {
	return !s.RefreshedAt.IsZero()
}
