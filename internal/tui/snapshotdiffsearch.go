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

	parent := diffParentOfDir(target)
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
	m = m.rebuildDiffRows(existingDiffDir(m.diffTree, origin), "")
	m.diffCursor = clampCursor(originCursor, len(m.diffRows))
	return m
}

func (m Model) restoreDiffSearchOrigin() Model {
	if !m.diffSearchJumped {
		return m
	}
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
	rowsByPath := make(map[string]model.DiffRow, len(entries))
	ranks := make([]model.FuzzyRank, 0, len(entries))
	for _, e := range entries {
		if e.Kinds&filter == 0 {
			continue
		}
		if match, ok := model.FuzzyScore(e.Path, query); ok {
			row := model.DiffRow{
				Name:     e.Path,
				Path:     e.Path,
				Type:     e.Type,
				Kinds:    e.Kinds,
				Modifier: e.Modifier,
				IsDir:    e.IsDir,
			}
			rowsByPath[e.Path] = row
			ranks = append(ranks, model.FuzzyRank{
				Entry: model.BrowseEntry{Name: e.Path, Path: e.Path, IsDir: e.IsDir},
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
