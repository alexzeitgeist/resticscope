package tui

import (
	"sort"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Diff search ranks parsed change entries in memory, excluding synthetic
// ancestors and repository contents, with the same ordering as browse search.

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
		return m.quitModel(), tea.Quit
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
	// Restore originCursor only in its original directory. After fallback to a
	// parent, normal rebuild restoration owns that different row list.
	if resolved == origin {
		m.diffCursor = clampCursor(originCursor, len(m.diffRows))
	}
	return m
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

// rankDiffSearchRows merges paths before filtering and fuzzy ranking. It returns
// at most limit rows and the total match count.
func rankDiffSearchRows(entries []model.DiffEntry, query string, filter model.ModifierKind, limit int) ([]model.DiffRow, int) {
	if strings.TrimSpace(query) == "" {
		return nil, 0
	}
	// Merge paths before filtering to match BuildDiffTree's OR-merge contract and
	// keep record order from changing multi-kind markers.
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
				// Derive duplicate markers from merged kinds, independent of stream order.
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
