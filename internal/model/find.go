package model

import (
	"sort"
	"time"
)

// FindMatch is one path match emitted by restic for a snapshot. Size and
// ModTime identify versions; the remaining fields provide display metadata.
type FindMatch struct {
	Path        string    `json:"path"`
	Name        string    `json:"name,omitempty"`
	Type        string    `json:"type,omitempty"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mtime"`
	Permissions string    `json:"permissions,omitempty"`
	UID         *uint32   `json:"uid,omitempty"`
	GID         *uint32   `json:"gid,omitempty"`
}

// FindSnapshotResult contains one snapshot's `restic find --json --long` hits.
type FindSnapshotResult struct {
	SnapshotID string      `json:"snapshot"`
	Hits       int         `json:"hits"`
	Matches    []FindMatch `json:"matches"`
}

// FileVersionOccurrence identifies a snapshot containing a file version. Its
// display metadata comes from the cached snapshot list.
type FileVersionOccurrence struct {
	SnapshotID string
	ShortID    string
	SnapTime   time.Time
	Hostname   string
}

// FileVersion groups matches by size and modification time, with occurrences
// newest first. Permissions and ownership are representative display metadata,
// not part of the version key. OwnerKnown distinguishes 0:0 from omitted
// ownership.
type FileVersion struct {
	Size        int64
	ModTime     time.Time
	Permissions string
	UID, GID    uint32
	OwnerKnown  bool
	Occurrences []FileVersionOccurrence
}

// fileVersionKey uses UTC so equal instants from different locations group
// together.
type fileVersionKey struct {
	size  int64
	mtime time.Time
}

// GroupFileVersions groups exact regular-file and untyped matches by size and
// modification time. Snapshot metadata is joined when available; occurrences
// and groups are sorted newest first.
func GroupFileVersions(results []FindSnapshotResult, literalPath string, snapByID map[string]Snapshot) []FileVersion {
	if len(results) == 0 {
		return nil
	}
	groups := make(map[fileVersionKey]*FileVersion)
	order := make([]fileVersionKey, 0)
	for _, r := range results {
		snap, snapKnown := snapByID[r.SnapshotID]
		for _, m := range r.Matches {
			// Drop matches whose path is not the literal we asked for, so foreign
			// glob hits never inflate a version group.
			if m.Path != literalPath {
				continue
			}
			// Reject matches identified as non-files before find-version extraction.
			// Untyped matches remain accepted but cannot attest a regular-file source.
			if m.Type != "" && m.Type != NodeTypeFile {
				continue
			}
			key := fileVersionKey{size: m.Size, mtime: m.ModTime.UTC()}
			occ := FileVersionOccurrence{SnapshotID: r.SnapshotID}
			if snapKnown {
				occ.ShortID = snap.ShortID
				occ.SnapTime = snap.Time
				occ.Hostname = snap.Hostname
			}
			g, ok := groups[key]
			if !ok {
				g = &FileVersion{Size: m.Size, ModTime: m.ModTime}
				groups[key] = g
				order = append(order, key)
			}
			// Fill missing display metadata from any occurrence in the group.
			if g.Permissions == "" {
				g.Permissions = m.Permissions
			}
			if !g.OwnerKnown && m.UID != nil && m.GID != nil {
				g.UID, g.GID, g.OwnerKnown = *m.UID, *m.GID, true
			}
			g.Occurrences = append(g.Occurrences, occ)
		}
	}
	if len(groups) == 0 {
		return nil
	}
	out := make([]FileVersion, 0, len(order))
	for _, k := range order {
		g := groups[k]
		// Stable sorting leaves unknown snapshot times at the bottom.
		sort.SliceStable(g.Occurrences, func(i, j int) bool {
			return g.Occurrences[i].SnapTime.After(g.Occurrences[j].SnapTime)
		})
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Sort groups by their latest known occurrence.
		return latestOccurrenceTime(out[i]).After(latestOccurrenceTime(out[j]))
	})
	return out
}

func latestOccurrenceTime(v FileVersion) time.Time {
	if len(v.Occurrences) == 0 {
		return time.Time{}
	}
	return v.Occurrences[0].SnapTime
}
