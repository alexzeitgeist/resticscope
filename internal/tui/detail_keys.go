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
		// b (and enter, as its drill-in alias) opens the in-app file browser for
		// the selected snapshot, kicking off the one-time index of its namespace.
		if snap := m.selectedSnapshot(); snap != nil {
			if name, ok := m.actionRepo(); ok {
				m.statusMsg = ""
				var cmd tea.Cmd
				m, cmd = m.startBrowse(name, snap.ID)
				return m, cmd
			}
		}
	case key.Matches(msg, m.keys.Mark):
		// t toggles the 2-slot FIFO mark on the cursor snapshot. Re-marking the
		// same row clears it; a third mark evicts the oldest. The marks live for
		// the detail context (cleared by goBack on the way back to the list).
		m.statusMsg = ""
		m = m.toggleDetailMark()
	case key.Matches(msg, m.keys.Diff):
		// d resolves the (older, newer) pair from the FIFO + cursor and opens the
		// diff view. With zero marks (or a 1-mark + same-cursor degenerate pair)
		// it surfaces a footer hint and stays put. diffPair normalizes its own
		// view of detailMarks; upstream mutators (group/collapse/refresh) already
		// keep m.detailMarks normalized, so no persistent prune is needed here.
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
		// i opens the full snapshot-info modal for the cursor snapshot. With an
		// empty repo it surfaces the same kind of hint the diff arm uses.
		if m.selectedSnapshot() != nil {
			m.statusMsg = ""
			m.infoScroll = 0
			m.view = infoView
		} else {
			m.statusMsg = "no snapshot selected"
		}
	case key.Matches(msg, m.keys.Extract):
		// e extracts the WHOLE selected snapshot (Source "/") through the same
		// modal browse uses for files/dirs; leaving the modal returns here.
		var cmd tea.Cmd
		m, cmd = m.openExtractSnapshot()
		return m, cmd
	case key.Matches(msg, m.keys.Group):
		// g cycles the detail snapshot table's section grouping
		// (off → host → tags → paths → off). cycleSnapGroup anchors on the
		// selected node's head ID so the same snapshot stays selected across
		// the reorder, then normalizes detail marks against the new display
		// (a previously visible mark on a now-hidden peer shows on its
		// absorbing head).
		m.statusMsg = ""
		m = m.cycleSnapGroup()
	case key.Matches(msg, m.keys.Collapse):
		// c toggles tree-ID collapse on the snapshot table. Same anchor +
		// normalize discipline as cycleSnapGroup; collapse-on can fold peer
		// marks into a head mark, collapse-off cannot un-merge them (folded
		// peers share an identity by construction).
		m.statusMsg = ""
		m = m.cycleSnapCollapse()
	}
	return m, nil
}
