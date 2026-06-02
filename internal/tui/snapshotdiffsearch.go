package tui

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/model"
)

// snapshotdiffsearch.go is the in-memory fuzzy search over the currently loaded
// diff. It deliberately searches only parsed change entries (not synthetic
// ancestor dirs, and never repository contents) and uses the same model.FuzzyScore
// / model.BetterFuzzy ordering as browse search.

const diffSearchResultLimit = browseSearchResultLimit

func (m Model) openDiffSearch() Model {
	m = m.exitDiffSearch()
	m.diffSearching = true
	m.diffSearchOrigin = m.diffDir
	m.diffSearchOrigCur = m.diffCursor
	if m.diffSearchOrigin == "" {
		m.diffSearchOrigin = model.DiffRoot
	}
	return m
}

func (m Model) handleDiffSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case key.Matches(msg, m.keys.SearchAccept):
		return m.acceptDiffSearch(), nil
	case key.Matches(msg, m.keys.SearchCancel):
		return m.cancelDiffSearch(), nil
	case key.Matches(msg, m.keys.SearchUp):
		if m.diffSearchCursor > 0 {
			m.diffSearchCursor--
		}
		return m, nil
	case key.Matches(msg, m.keys.SearchDown):
		if m.diffSearchCursor < len(m.diffSearchRows)-1 {
			m.diffSearchCursor++
		}
		return m, nil
	case key.Matches(msg, m.keys.PageUp):
		m.diffSearchCursor = clampCursor(m.diffSearchCursor-m.diffVisible(), len(m.diffSearchRows))
		return m, nil
	case key.Matches(msg, m.keys.PageDown):
		m.diffSearchCursor = clampCursor(m.diffSearchCursor+m.diffVisible(), len(m.diffSearchRows))
		return m, nil
	case key.Matches(msg, m.keys.FilterDelete):
		if r := []rune(m.diffSearchQuery); len(r) > 0 {
			m.diffSearchQuery = string(r[:len(r)-1])
			return m.fireDiffSearch(), nil
		}
		return m, nil
	default:
		if msg.Text != "" {
			m.diffSearchQuery += msg.Text
			return m.fireDiffSearch(), nil
		}
		return m, nil
	}
}

func (m Model) fireDiffSearch() Model {
	m.diffSearchRows, m.diffSearchTotal = rankDiffSearchRows(m.diffEntries, m.diffSearchQuery, m.diffFilters, diffSearchResultLimit)
	m.diffSearchCursor = 0
	return m
}

func (m Model) acceptDiffSearch() Model {
	r := m.selectedDiffSearchRow()
	if r == nil {
		return m.cancelDiffSearch()
	}
	target := r.Path
	origin, originCursor := m.diffSearchOrigin, m.diffSearchOrigCur
	m = m.exitDiffSearch()
	m.diffSearchOrigin = origin
	m.diffSearchOrigCur = originCursor
	m.diffSearchJumped = true

	parent := model.DiffParentOf(target)
	if parent == "" {
		parent = model.DiffRoot
	}
	return m.rebuildDiffRows(existingDiffDir(m.diffTree, parent), target)
}

func (m Model) cancelDiffSearch() Model {
	origin, originCursor := m.diffSearchOrigin, m.diffSearchOrigCur
	m = m.exitDiffSearch()
	if origin == "" {
		origin = model.DiffRoot
	}
	resolved := existingDiffDir(m.diffTree, origin)
	m = m.rebuildDiffRows(resolved, "")
	// Only restore the origin cursor when we actually landed in the origin dir.
	// If existingDiffDir fell back to a parent (filter/swap removed origin),
	// originCursor indexes the wrong list — let rebuildDiffRows' normal cursor
	// restoration (selectPath / diffCache / 0) own the fallback dir's position.
	if resolved == origin {
		m.diffCursor = clampCursor(originCursor, len(m.diffRows))
	}
	return m
}

// restoreDiffSearchOrigin reverses an accepted search jump: every caller has
// already gated on m.diffSearchJumped, so this delegates to the same teardown
// cancelDiffSearch performs (rebuild at origin, clamp cursor).
func (m Model) restoreDiffSearchOrigin() Model {
	return m.cancelDiffSearch()
}

func (m Model) exitDiffSearch() Model {
	m.diffSearching = false
	m.diffSearchQuery = ""
	m.diffSearchRows = nil
	m.diffSearchCursor = 0
	m.diffSearchTotal = 0
	m.diffSearchOrigin = ""
	m.diffSearchOrigCur = 0
	m.diffSearchJumped = false
	return m
}

func (m Model) selectedDiffSearchRow() *model.DiffRow {
	if m.diffSearchCursor < 0 || m.diffSearchCursor >= len(m.diffSearchRows) {
		return nil
	}
	return &m.diffSearchRows[m.diffSearchCursor]
}

func rankDiffSearchRows(entries []model.DiffEntry, query string, filter model.ModifierKind, limit int) ([]model.DiffRow, int) {
	if strings.TrimSpace(query) == "" {
		return nil, 0
	}
	// Pre-merge by path: a `M`+`U` pair for the same file must rank as one MU
	// row to match BuildDiffTree's per-path OR-merge contract. Filtering or
	// first-wins-deduping before the merge would let the active filter (or
	// stream order) change which record's marker the row renders.
	type merged struct {
		kinds    model.ModifierKind
		modifier string
		isDir    bool
		dup      bool
	}
	byPath := make(map[string]*merged, len(entries))
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if existing, ok := byPath[e.Path]; ok {
			existing.kinds |= e.Kinds
			existing.isDir = existing.isDir || e.IsDir
			existing.dup = true
			continue
		}
		byPath[e.Path] = &merged{kinds: e.Kinds, modifier: e.Modifier, isDir: e.IsDir}
		order = append(order, e.Path)
	}

	rowsByPath := make(map[string]model.DiffRow, len(order))
	ranks := make([]model.FuzzyRank, 0, len(order))
	for _, p := range order {
		mg := byPath[p]
		if mg.kinds&filter == 0 {
			continue
		}
		if match, ok := model.FuzzyScore(p, query); ok {
			modifier := mg.modifier
			if mg.dup {
				// A duplicate-path merge has no canonical "original" string;
				// render from the OR-merged Kinds so the marker is the same
				// regardless of which order restic emitted the records.
				modifier = model.ModifierString(mg.kinds)
			}
			row := model.DiffRow{
				Name:     p,
				Path:     p,
				Type:     model.PrimaryChangeType(mg.kinds),
				Kinds:    mg.kinds,
				Modifier: modifier,
				IsDir:    mg.isDir,
			}
			rowsByPath[p] = row
			ranks = append(ranks, model.FuzzyRank{
				Entry: model.BrowseEntry{Name: p, Path: p, IsDir: mg.isDir},
				Match: match,
			})
		}
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		return model.BetterFuzzy(ranks[i], ranks[j])
	})
	total := len(ranks)
	if limit > 0 && len(ranks) > limit {
		ranks = ranks[:limit]
	}
	out := make([]model.DiffRow, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, rowsByPath[r.Entry.Path])
	}
	return out, total
}
