package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m Model) handleListKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Any list-view key dismisses a prior transient footer notice (e.g.
	// "grouping not configured") so it can't persist past the user's next
	// interaction. Handlers that want to surface a new notice (cycleGrouping
	// on a missing config) set m.statusMsg after this clear runs.
	m.statusMsg = ""
	switch {
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.displayList().rows)-1 {
			m.cursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.cursor = clampCursor(m.cursor-m.visibleRepos(), len(m.displayList().rows))
	case key.Matches(msg, m.keys.PageDown):
		m.cursor = clampCursor(m.cursor+m.visibleRepos(), len(m.displayList().rows))
	case key.Matches(msg, m.keys.Enter):
		// Pin the detail view to the selected repo by name so a later refresh
		// (which can reorder an urgency sort) can't swap it out.
		if row, ok := m.currentRow(); ok {
			m.detailName = row.Name
			m.view = detailView
			m.snapCursor = 0
			// Reset transient group/collapse state every detail entry. goBack
			// also resets them on exit, but be explicit so a future code path
			// that lands on detail without going through goBack can't inherit
			// stale values. Collapse is opt-in (rare to help in the flat
			// newest-first stream); group cycles via `g`.
			m.snapGroupMode = snapGroupOff
			m.snapCollapseTree = false
		}
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
	case key.Matches(msg, m.keys.Sort):
		m = m.cycleSort()
	case key.Matches(msg, m.keys.Group):
		m = m.cycleGrouping()
	}
	return m, nil
}

// handleFilterKey consumes keys while the filter input is open. Apply keeps the
// query and returns to normal navigation; clear (esc) drops the query entirely;
// ctrl+c still quits. Every other key edits the query text. The cursor resets to
// the top whenever the query changes so it never points past the matches.
func (m Model) handleFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case key.Matches(msg, m.keys.FilterAccept):
		m.filtering = false
	case key.Matches(msg, m.keys.FilterCancel):
		m.filtering = false
		m.filter = ""
		m.cursor = 0
	case key.Matches(msg, m.keys.FilterDelete):
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
			m.cursor = 0
		}
	default:
		// Text is non-empty only for printable keys, so this ignores stray
		// control keys (arrows, etc.) rather than inserting garbage.
		if msg.Text != "" {
			m.filter += msg.Text
			m.cursor = 0
		}
	}
	return m, nil
}

// cycleSort advances to the next sort mode, keeping the cursor on the same repo
// across the reorder. Clearing a stale statusMsg is handled centrally by
// handleListKey, so this body only owns the sort + cursor reanchor.
func (m Model) cycleSort() Model {
	var sel string
	if row, ok := m.currentRow(); ok {
		sel = row.Name
	}
	m.sortMode = (m.sortMode + 1) % sortModeCount
	m.cursor = m.indexOf(sel)
	return m
}

// cycleGrouping advances the grouping cycle: each press steps to the next
// configured key, then to the flat view, then wraps. The cursor stays on the
// same repo across the reorder by anchoring on its name. When no keys are
// configured it is a no-op that surfaces a transient footer notice so the
// user understands why nothing happened — set after handleListKey's central
// clear runs so the new notice survives this turn.
func (m Model) cycleGrouping() Model {
	if !m.groupingConfigured() {
		m.statusMsg = "grouping not configured"
		return m
	}
	var sel string
	if row, ok := m.currentRow(); ok {
		sel = row.Name
	}
	m.groupIndex = (m.groupIndex + 1) % (len(m.app.Cfg.Global.GroupBy) + 1)
	m.cursor = m.indexOf(sel)
	return m
}
