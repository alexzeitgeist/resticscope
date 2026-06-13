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
	ID             string           `json:"id"`
	ShortID        string           `json:"short_id"`
	Time           time.Time        `json:"time"`
	Hostname       string           `json:"hostname"`
	Username       string           `json:"username,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	ProgramVersion string           `json:"program_version,omitempty"`
	Parent         string           `json:"parent,omitempty"`   // parent snapshot id (chain link)
	Tree           string           `json:"tree,omitempty"`     // root tree blob id
	Paths          []string         `json:"paths,omitempty"`    // source paths included in the backup
	Excludes       []string         `json:"excludes,omitempty"` // excluded paths/globs recorded by restic
	UID            *uint32          `json:"uid,omitempty"`      // owner uid; pointer preserves root uid 0
	GID            *uint32          `json:"gid,omitempty"`      // owner gid; pointer preserves root gid 0
	Summary        *SnapshotSummary `json:"summary,omitempty"`
}

// SnapshotSummary mirrors the `summary` object restic 0.17+ returns inline with
// each entry of `restic snapshots --json`. It gives a per-snapshot size for
// free, so resticscope no longer runs a separate (heavy) `restic stats`.
//
// The field is a pointer on Snapshot so a missing summary (a pre-0.17 snapshot)
// is distinguishable from a genuine zero-byte snapshot.
type SnapshotSummary struct {
	TotalBytesProcessed int64     `json:"total_bytes_processed"`           // logical size of this snapshot
	DataAdded           *int64    `json:"data_added,omitempty"`            // new (deduped) bytes this run added
	DataAddedPacked     *int64    `json:"data_added_packed,omitempty"`     // actual repo growth this run (compressed)
	BackupStart         time.Time `json:"backup_start,omitzero"`           // when this run began
	BackupEnd           time.Time `json:"backup_end,omitzero"`             // when this run finished; End-Start is the duration
	FilesNew            *uint64   `json:"files_new,omitempty"`             // files added this run
	FilesChanged        *uint64   `json:"files_changed,omitempty"`         // files modified this run
	FilesUnmodified     *uint64   `json:"files_unmodified,omitempty"`      // files unchanged this run
	DirsNew             *uint64   `json:"dirs_new,omitempty"`              // directories added this run
	DirsChanged         *uint64   `json:"dirs_changed,omitempty"`          // directories modified this run
	DirsUnmodified      *uint64   `json:"dirs_unmodified,omitempty"`       // directories unchanged this run
	DataBlobs           *int64    `json:"data_blobs,omitempty"`            // new data blobs written
	TreeBlobs           *int64    `json:"tree_blobs,omitempty"`            // new tree blobs written
	TotalFilesProcessed *uint64   `json:"total_files_processed,omitempty"` // files in the snapshot
}
