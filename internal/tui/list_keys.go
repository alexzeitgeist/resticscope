package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m Model) handleListKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Dismiss prior notices before handlers optionally set a new one for this
	// interaction.
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
			// Reset transient detail state here as well as in goBack so alternate
			// entry paths cannot inherit it.
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

// handleFilterKey consumes input until apply or cancel; ctrl+c still quits.
// Query changes reset the cursor so it cannot point beyond the filtered rows.
func (m Model) handleFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		return m.quitModel(), tea.Quit
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
		// Key text excludes control keys, so only printable input is appended.
		if msg.Text != "" {
			m.filter += msg.Text
			m.cursor = 0
		}
	}
	return m, nil
}

// cycleSort advances the sort mode and reanchors the cursor by repository name.
// handleListKey clears transient notices.
func (m Model) cycleSort() Model {
	var sel string
	if row, ok := m.currentRow(); ok {
		sel = row.Name
	}
	m.sortMode = (m.sortMode + 1) % sortModeCount
	m.cursor = m.indexOf(sel)
	return m
}

// cycleGrouping advances through configured keys and the flat view while
// reanchoring the cursor by repository name. With no keys it reports a
// transient notice after handleListKey's central clear.
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
