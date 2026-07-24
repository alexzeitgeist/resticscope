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
	case key.Matches(msg, m.keys.Browse), key.Matches(msg, m.keys.Enter):
		// Browse and Enter both start the selected snapshot's one-time index.
		if snap := m.selectedSnapshot(); snap != nil {
			if name, ok := m.actionRepo(); ok {
				m.statusMsg = ""
				var cmd tea.Cmd
				m, cmd = m.startBrowse(name, snap.ID)
				return m, cmd
			}
		}
	case key.Matches(msg, m.keys.Mark):
		// Marks form a two-slot FIFO, toggle off on repeat, and clear on leaving detail.
		m.statusMsg = ""
		m = m.toggleDetailMark()
	case key.Matches(msg, m.keys.Diff):
		// Open the normalized mark/cursor pair, or stay put with a hint when the
		// pair is missing or degenerate.
		if name, ok := m.actionRepo(); ok {
			if older, newer, pairOK := m.diffPair(); pairOK {
				m.statusMsg = ""
				var cmd tea.Cmd
				m, cmd = m.startSnapshotDiff(name, older, newer)
				return m, cmd
			}
			m.statusMsg = "mark snapshots with t"
		}
	case key.Matches(msg, m.keys.Info):
		// Empty repositories stay in detail with a hint.
		if m.selectedSnapshot() != nil {
			m.statusMsg = ""
			m.infoScroll = 0
			m.view = infoView
		} else {
			m.statusMsg = "no snapshot selected"
		}
	case key.Matches(msg, m.keys.Extract):
		// Whole-snapshot extraction shares browse's modal and returns to detail.
		var cmd tea.Cmd
		m, cmd = m.openExtractSnapshot()
		return m, cmd
	case key.Matches(msg, m.keys.Group):
		// Grouping anchors on the selected head and folds hidden peer marks into
		// their displayed head.
		m.statusMsg = ""
		m = m.cycleSnapGroup()
	case key.Matches(msg, m.keys.Collapse):
		// Collapse shares grouping's anchor and mark normalization; folded peer
		// identities cannot be separated again on expansion.
		m.statusMsg = ""
		m = m.cycleSnapCollapse()
	}
	return m, nil
}
