package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m Model) handleListKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.visibleRows())-1 {
			m.cursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.cursor = clampCursor(m.cursor-m.visibleRepos(), len(m.visibleRows()))
	case key.Matches(msg, m.keys.PageDown):
		m.cursor = clampCursor(m.cursor+m.visibleRepos(), len(m.visibleRows()))
	case key.Matches(msg, m.keys.Enter):
		// Pin the detail view to the selected repo by name so a later refresh
		// (which can reorder a size/staleness sort) can't swap it out.
		if row, ok := m.currentRow(); ok {
			m.detailName = row.Name
			m.view = detailView
			m.snapCursor = 0
		}
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
	case key.Matches(msg, m.keys.Sort):
		m = m.cycleSort()
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
// across the reorder.
func (m Model) cycleSort() Model {
	var sel string
	if row, ok := m.currentRow(); ok {
		sel = row.Name
	}
	m.sortMode = (m.sortMode + 1) % sortModeCount
	m.cursor = m.indexOf(sel)
	return m
}
