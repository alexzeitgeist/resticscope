package model

import (
	"sort"
	"time"
)

// find.go holds the pure DTOs and grouping helper behind the "show me other
// versions of this file across snapshots" view (see internal/tui/findversions.go).
// Like the rest of model, it has no internal imports: it is the leaf the
// resticx/app/tui chain decodes into and reads from.

// FindMatch is one match restic emitted for the queried path, inside one
// snapshot. Of these fields, only Size and ModTime are used as the dedup key;
// the rest are kept for completeness so a future inline detail panel can
// render them without a second restic call.
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

// FindSnapshotResult is one snapshot's hits, as emitted by `restic find
// --json --long`.
type FindSnapshotResult struct {
	SnapshotID string      `json:"snapshot"`
	Hits       int         `json:"hits"`
	Matches    []FindMatch `json:"matches"`
}

// FileVersionOccurrence is one snapshot that carried a given version of the
// file. The snapshot fields (ShortID/SnapTime/Hostname) are joined in from
// the cached snapshot list so the version view never calls `restic
// snapshots` itself.
type FileVersionOccurrence struct {
	SnapshotID string
	ShortID    string
	SnapTime   time.Time
	Hostname   string
}

// FileVersion is one distinct version of the file (collapsed by (Size,
// ModTime)) plus the snapshots that contain it, newest-first. Permissions /
// UID / GID are representative display metadata captured from any match in the
// group that carries them; they never participate in dedup (a chmod or chown
// that does not bump mtime keeps the file in the same version group, matching
// restic's own content key). OwnerKnown distinguishes a match that carried
// uid/gid (a real 0:0 root-owned file) from a match that did not, mirroring
// model.BrowseEntry.OwnerKnown.
type FileVersion struct {
	Size        int64
	ModTime     time.Time
	Permissions string
	UID, GID    uint32
	OwnerKnown  bool
	Occurrences []FileVersionOccurrence
}

// fileVersionKey is the dedup tuple over (Size, ModTime). ModTime is reduced
// to UnixNano so equivalent instants with different time.Location pointers
// still group together; time.Time == would split them.
type fileVersionKey struct {
	size  int64
	mtime int64
}

// GroupFileVersions collapses restic find's flat per-snapshot list into
// distinct (Size, ModTime) versions. It first post-filters every match to
// Path == literalPath so a glob hit from a sibling file (restic find treats
// PATTERN as a filepath.Match glob — see resticx.resticFindLiteralPattern) is
// dropped before grouping; this filter is the authoritative correctness gate,
// the escape in resticx is just the cost-saver. snapByID supplies each
// snapshot's ShortID/Time/Hostname; a missing entry yields an occurrence with
// only the SnapshotID filled, never an error. Within a group occurrences are
// sorted newest-first; across groups the result is sorted by the group's
// latest occurrence newest-first, so the most recent version is on top.
func GroupFileVersions(results []FindSnapshotResult, literalPath string, snapByID map[string]Snapshot) []FileVersion {
	if len(results) == 0 {
		return nil
	}
	groups := make(map[fileVersionKey]*FileVersion)
	order := make([]fileVersionKey, 0)
	for _, r := range results {
		snap, snapKnown := snapByID[r.SnapshotID]
		for _, m := range r.Matches {
			// to ensure foreign glob hits never inflate a version group: drop
			// any match whose Path is not exactly the literal we asked for.
			if m.Path != literalPath {
				continue
			}
			key := fileVersionKey{size: m.Size, mtime: m.ModTime.UnixNano()}
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
			// Fill display metadata from any occurrence that carries it, not
			// just the first: mixed-restic-version or mixed-filesystem repos
			// can emit one match without uid/gid (or permissions) and another
			// with them for the same (size,mtime) content.
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
		// Sort occurrences newest-first inside the group; an unknown SnapTime
		// (zero) sorts after known times so missing-metadata rows fall to the bottom.
		sort.SliceStable(g.Occurrences, func(i, j int) bool {
			return g.Occurrences[i].SnapTime.After(g.Occurrences[j].SnapTime)
		})
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Sort groups by their latest occurrence's snapshot time newest-first;
		// groups with no known snapshot times sort last but stably.
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
