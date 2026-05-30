package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m Model) handleDetailKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		m = m.goBack()
	case key.Matches(msg, m.keys.Up):
		if m.snapCursor > 0 {
			m.snapCursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.snapCursor < m.snapCount()-1 {
			m.snapCursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.snapCursor = clampCursor(m.snapCursor-m.detailSnapVisible(), m.snapCount())
	case key.Matches(msg, m.keys.PageDown):
		m.snapCursor = clampCursor(m.snapCursor+m.detailSnapVisible(), m.snapCount())
	case key.Matches(msg, m.keys.Browse):
		// b opens the in-app file browser for the selected snapshot, kicking off
		// the one-time index of its namespace.
		if snap := m.selectedSnapshot(); snap != nil {
			if name, ok := m.actionRepo(); ok {
				m.statusMsg = ""
				var cmd tea.Cmd
				m, cmd = m.startBrowse(name, snap.ID)
				return m, cmd
			}
		}
	case key.Matches(msg, m.keys.Enter):
		// Enter on a snapshot shells in with that snapshot's context.
		if snap := m.selectedSnapshot(); snap != nil {
			if cmd := m.openShellCmd(snap); cmd != nil {
				m.statusMsg = ""
				return m, cmd
			}
		}
	}
	return m, nil
}
