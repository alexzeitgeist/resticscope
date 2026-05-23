package tui

import (
	"sort"
	"strings"

	"resticscope/internal/app"
)

// sortMode orders the list view. sortConfig is the natural config order; the
// other modes surface the repos most likely to need attention (oldest backup,
// or largest on disk) at the top.
type sortMode int

const (
	sortConfig    sortMode = iota // config order (default)
	sortStale                     // oldest last snapshot first (never-refreshed first)
	sortSize                      // largest total size first
	sortModeCount                 // sentinel: number of modes, for cycling
)

// label is the human name shown in the header when a non-default sort is active.
func (s sortMode) label() string {
	switch s {
	case sortStale:
		return "staleness"
	case sortSize:
		return "size"
	default:
		return "config"
	}
}

// sortRows orders rows in place per mode. It is stable, so repos with equal sort
// keys keep their config order. sortConfig leaves the slice untouched.
func sortRows(rows []app.RepoStatus, mode sortMode) {
	switch mode {
	case sortStale:
		// Oldest backup at the top. A never-refreshed repo has a zero
		// LastSnapshot, which sorts before any real time — i.e. most stale.
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].State.LastSnapshot.Before(rows[j].State.LastSnapshot)
		})
	case sortSize:
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].State.TotalSize > rows[j].State.TotalSize
		})
	}
}

// matchRepo reports whether a repo matches the filter query q, which must be
// lowercased and trimmed by the caller. An empty query matches everything. The
// query is tested as a case-insensitive substring of the repo name, its
// credential's region, and each of its label values — the same facts shown on
// the list's meta line.
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

// visibleRows is the configured rows with the active filter and sort applied.
// The list cursor and every list-view action index into this, never m.rows, so
// what the user selects is always what they see. m.rows itself stays in config
// order so refresh-all and the coverage rollup keep spanning every repo.
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

// currentRow returns the row under the list cursor from the visible (filtered,
// sorted) rows. ok is false when the filter matches nothing. The cursor is
// clamped on read so a filter that shrinks the list can never index out of
// range.
func (m Model) currentRow() (app.RepoStatus, bool) {
	rows := m.visibleRows()
	if len(rows) == 0 {
		return app.RepoStatus{}, false
	}
	cur := m.cursor
	if cur < 0 {
		cur = 0
	}
	if cur >= len(rows) {
		cur = len(rows) - 1
	}
	return rows[cur], true
}

// indexOf returns the visible-row index of the named repo, or 0 if it is not
// currently visible. It is used to keep the cursor on the same repo across a
// sort change.
func (m Model) indexOf(name string) int {
	for i, r := range m.visibleRows() {
		if r.Name == name {
			return i
		}
	}
	return 0
}
