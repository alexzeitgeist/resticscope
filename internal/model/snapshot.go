// Package model holds the pure domain types shared across resticscope.
//
// It is the leaf of the dependency graph: it imports nothing from internal/.
// Everything here must be safe to marshal to the on-disk cache, which means it
// must never carry credentials (see internal/cache and the engineering rules).
package model

import "time"

// Snapshot mirrors the subset of `restic snapshots --json` we care about.
//
// restic's JSON contract is additive, so unknown fields are ignored on decode.
// Keep this struct tolerant: never fail a refresh because restic grew a field.
type Snapshot struct {
	ID             string    `json:"id"`
	ShortID        string    `json:"short_id"`
	Time           time.Time `json:"time"`
	Hostname       string    `json:"hostname"`
	Username       string    `json:"username,omitempty"`
	Paths          []string  `json:"paths,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	ProgramVersion string    `json:"program_version,omitempty"`
}

// Stats mirrors `restic stats --json --mode raw-data`.
//
// restic reports blob counts, not pack counts; the field name reflects what
// restic actually returns. Stats are best-effort (slow on large repos) and a
// refresh stays useful even when they fail.
type Stats struct {
	TotalSize      int64 `json:"total_size"`
	TotalBlobCount int   `json:"total_blob_count"`
	SnapshotsCount int   `json:"snapshots_count"`
}
