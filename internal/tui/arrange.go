package tui

import (
	"sort"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// sortMode orders the list view; `o` cycles config, urgency, and name.
type sortMode int

const (
	sortConfig    sortMode = iota // config order (default)
	sortUrgency                   // most urgent first (error -> red -> amber -> green -> grey)
	sortName                      // repo name, case-insensitive A-Z
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

// urgencyRank maps status to display urgency. Grey follows green because an
// unrefreshed repository has no failed-freshness evidence.
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

// browseSortMode orders a browser directory without changing list-view order.
// Every mode keeps directories before files.
type browseSortMode int

const (
	browseSortName      browseSortMode = iota // canonical store order (default)
	browseSortSize                            // largest first
	browseSortModified                        // newest first
	browseSortModeCount                       // sentinel: number of modes, for cycling
)

// label names a non-default browse sort in the summary.
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

// sortedBrowseRows returns canonical directly in name mode and a sorted copy in
// other modes. It never mutates browseCache, and callers must not mutate the
// returned slice.
func sortedBrowseRows(canonical []model.BrowseEntry, mode browseSortMode) []model.BrowseEntry {
	if mode == browseSortName {
		return canonical
	}
	rows := make([]model.BrowseEntry, len(canonical))
	copy(rows, canonical)
	sort.SliceStable(rows, browseLess(rows, mode))
	return rows
}

// browseLess orders directories first, then the selected key, lowercase name,
// exact name, and unique path. Its name ordering matches the database's
// lowercase-name SQLite BINARY collation, making sort cycling deterministic.
func browseLess(rows []model.BrowseEntry, mode browseSortMode) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		switch mode {
		case browseSortSize:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		case browseSortModified:
			// Unknown (zero) mtimes sort last: a known time orders before an unknown
			// one, and two unknowns fall through to the name tie-break.
			if a.ModTime.IsZero() != b.ModTime.IsZero() {
				return !a.ModTime.IsZero()
			}
			if !a.ModTime.IsZero() && !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.After(b.ModTime)
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

func normalizedFilter(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// matchRepo reports whether q occurs in a repository name, credential region,
// or label. Empty queries match everything; region remains searchable despite
// no longer being displayed as a column.
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

// currentRow returns the displayed row under the clamped cursor. ok is false
// when filtering hides every row, and display ordering keeps the highlighted
// and acted-on repositories aligned.
func (m Model) currentRow() (app.RepoStatus, bool) {
	rows := m.displayList().rows
	if len(rows) == 0 {
		return app.RepoStatus{}, false
	}
	return rows[clampCursor(m.cursor, len(rows))], true
}

// indexOf returns a repository's display index, or zero when it is hidden. This
// keeps the cursor on the same repository when display ordering changes.
func (m Model) indexOf(name string) int {
	for i, r := range m.displayList().rows {
		if r.Name == name {
			return i
		}
	}
	return 0
}
