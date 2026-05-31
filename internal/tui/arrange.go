package tui

import (
	"sort"
	"strings"

	"resticscope/internal/app"
	"resticscope/internal/model"
)

// sortMode orders the list view. The `o` key cycles config → urgency → name →
// config; sortConfig is the natural config order.
type sortMode int

const (
	sortConfig    sortMode = iota // config order (default)
	sortUrgency                   // most urgent first (error → red → amber → green → grey)
	sortName                      // repo name, case-insensitive A→Z
	sortModeCount                 // sentinel: number of modes, for cycling
)

// label is the human name shown in the header when a non-default sort is active.
func (s sortMode) label() string {
	switch s {
	case sortUrgency:
		return "urgency"
	case sortName:
		return "name"
	default:
		return "config"
	}
}

// urgencyRank maps an evaluated status to its sort position for sortUrgency.
// Grey (never refreshed) is least urgent — it carries no failed freshness
// evidence yet, so it sorts after green. This is a product-ordering choice and
// is deliberately distinct from EvaluateStatus's internal precedence.
func urgencyRank(s model.Status) int {
	switch s {
	case model.StatusError:
		return 0
	case model.StatusRed:
		return 1
	case model.StatusAmber:
		return 2
	case model.StatusGreen:
		return 3
	case model.StatusGrey:
		return 4
	default:
		return 5
	}
}

// sortRows orders rows in place per mode. It is stable, so repos with equal sort
// keys keep their config order. sortConfig leaves the slice untouched.
func sortRows(rows []app.RepoStatus, mode sortMode) {
	switch mode {
	case sortUrgency:
		sort.SliceStable(rows, func(i, j int) bool {
			return urgencyRank(rows[i].Status) < urgencyRank(rows[j].Status)
		})
	case sortName:
		sort.SliceStable(rows, func(i, j int) bool {
			return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
		})
	}
}

// browseSortMode orders the snapshot browser's current directory listing. It is a
// transient TUI display mode for the active browse session, kept distinct from the
// list view's sortMode so the two sort idioms never get mixed. browseSortName is
// the canonical store order browsedb.ListDir returns (dirs first, case-insensitive
// name); size and modified reorder within the dirs/files groups.
type browseSortMode int

const (
	browseSortName      browseSortMode = iota // canonical store order (default)
	browseSortSize                            // largest first
	browseSortModified                        // newest first
	browseSortModeCount                       // sentinel: number of modes, for cycling
)

// label is the human name shown in the browse summary when a non-default sort is
// active.
func (s browseSortMode) label() string {
	switch s {
	case browseSortSize:
		return "size"
	case browseSortModified:
		return "modified"
	default:
		return "name"
	}
}

// sortedBrowseRows returns the rows in display order for mode. browseSortName is
// the canonical ListDir order, so it returns canonical unchanged — no copy, no
// reorder, so browseRows aliases the cached slice in name mode (it is never mutated
// in place), mirroring sortRows leaving sortConfig untouched. size/modified sort a
// COPY (sort.SliceStable) so the canonical cached slice (browseCache) is never
// mutated. Net: the cache is never mutated in any mode.
func sortedBrowseRows(canonical []model.BrowseEntry, mode browseSortMode) []model.BrowseEntry {
	if mode == browseSortName {
		return canonical
	}
	rows := make([]model.BrowseEntry, len(canonical))
	copy(rows, canonical)
	sort.SliceStable(rows, browseLess(rows, mode))
	return rows
}

// browseLess is the comparator used with sort.SliceStable (matching arrange.go's
// sortRows): dirs first; then by the mode's key; tie-break ToLower(name) → name →
// Path. The name tie-break is byte-identical to the DB's name_ci, name ordering
// (name_ci = strings.ToLower(name), and Go string < matches SQLite BINARY), so it
// reproduces canonical order for equal keys. The unique Path final tie-break makes
// the order total for a directory listing (paths are unique), so cycling is
// deterministic; SliceStable keeps any exact tie in canonical order.
func browseLess(rows []model.BrowseEntry, mode browseSortMode) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // dirs first in every mode
		}
		switch mode {
		case browseSortSize:
			if a.Size != b.Size {
				return a.Size > b.Size // largest first
			}
		case browseSortModified:
			// Unknown (zero) mtimes sort last: a known time orders before an unknown
			// one, and two unknowns fall through to the name tie-break.
			if a.ModTime.IsZero() != b.ModTime.IsZero() {
				return !a.ModTime.IsZero()
			}
			if !a.ModTime.IsZero() && !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.After(b.ModTime) // newest first
			}
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Path < b.Path
	}
}

// matchRepo reports whether a repo matches the filter query q, which must be
// lowercased and trimmed by the caller. An empty query matches everything. The
// query is tested as a case-insensitive substring of the repo name, its
// credential's region, and each of its label values — covering both the Name
// column and the Labels column the list view surfaces. Region is matched even
// though it is no longer a column so a region-based filter keeps working.
func matchRepo(name string, meta rowMeta, q string) bool {
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(meta.region), q) {
		return true
	}
	for _, l := range meta.labels {
		if strings.Contains(strings.ToLower(l), q) {
			return true
		}
	}
	return false
}

// visibleRows is the configured rows with the active filter and sort applied,
// flattened to a single slice without group sectioning. It backs the header's
// filter count and page-jump sizing; in grouped mode it's only an
// approximation of the displayed order because displayList() partitions the
// same rows into sections. The cursor and every list-view action index into
// displayList().rows — the canonical render-and-action order — not into this
// slice. m.rows itself stays in config order so refresh-all keeps spanning
// every repo.
func (m Model) visibleRows() []app.RepoStatus {
	q := strings.ToLower(strings.TrimSpace(m.filter))
	rows := make([]app.RepoStatus, 0, len(m.rows))
	for _, r := range m.rows {
		if matchRepo(r.Name, m.meta[r.Name], q) {
			rows = append(rows, r)
		}
	}
	sortRows(rows, m.sortMode)
	return rows
}

// currentRow returns the row under the list cursor in the canonical display
// order — the same flattened slice rendering uses, so the highlighted repo and
// the acted-on repo can never diverge. ok is false when the filter matches
// nothing. The cursor is clamped on read so a filter that shrinks the list can
// never index out of range.
func (m Model) currentRow() (app.RepoStatus, bool) {
	rows := m.displayList().rows
	if len(rows) == 0 {
		return app.RepoStatus{}, false
	}
	return rows[clampCursor(m.cursor, len(rows))], true
}

// indexOf returns the display-order index of the named repo, or 0 if it is not
// currently visible. It is used to keep the cursor on the same repo across a
// sort change, a grouping toggle, or a background refresh that reorders rows.
func (m Model) indexOf(name string) int {
	for i, r := range m.displayList().rows {
		if r.Name == name {
			return i
		}
	}
	return 0
}
