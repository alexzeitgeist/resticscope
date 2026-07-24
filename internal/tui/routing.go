package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleKey gives text input, global keys, and modal views priority before
// routing to the active view. Repeated q backs out of nested views and then
// quits from the list.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := m.handleInputKey(msg); handled {
		return next, cmd
	}
	if next, cmd, handled := m.handleGlobalKey(msg); handled {
		return next, cmd
	}
	if next, handled := m.handleModalViewKey(msg); handled {
		return next, nil
	}
	return m.handleViewKey(msg)
}

// handleInputKey gives active text inputs priority so printable global keys
// remain query text until input ends.
func (m Model) handleInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case m.filtering:
		next, cmd := m.handleFilterKey(msg)
		return next, cmd, true
	case m.browseSearching:
		next, cmd := m.handleBrowseSearchKey(msg)
		return next, cmd, true
	case m.diffSearching:
		next, cmd := m.handleDiffSearchKey(msg)
		return next, cmd, true
	}
	return m, nil, false
}

// handleGlobalKey handles universal keys, delegating q to cancel-aware nested
// back paths.
func (m Model) handleGlobalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		return m.quitModel(), tea.Quit, true
	case key.Matches(msg, m.keys.Quit):
		next, cmd := m.handleQuitKey()
		return next, cmd, true
	case key.Matches(msg, m.keys.Help):
		return m.toggleHelp(), nil, true
	}
	return m, nil, false
}

func (m Model) quitModel() Model {
	m.quitting = true
	if m.cancel != nil {
		m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
	}
	return m
}

// handleQuitKey implements q's dual role: it quits from the main list but steps
// back one screen from nested views, so repeated q walks home and then exits.
func (m Model) handleQuitKey() (tea.Model, tea.Cmd) {
	switch m.view {
	case extractView:
		// Delegate q to the extract model's state-specific Back behavior.
		next, cmd, _ := m.extract.back()
		m.extract = next
		return m, cmd
	case browseView:
		return m.browseBack(), nil
	case findVersionsView:
		return m.findVersionsBack(), nil
	case snapshotDiffView:
		// After a search jump, q mirrors esc by reversing the jump first.
		if m.diffSearchJumped {
			return m.cancelDiffSearch(), nil
		}
		return m.snapshotDiffBack(), nil
	case listView:
		return m.quitModel(), tea.Quit
	default:
		return m.goBack(), nil
	}
}

func (m Model) handleModalViewKey(msg tea.KeyPressMsg) (Model, bool) {
	switch m.view {
	case helpView:
		return m.handleHelpViewKey(msg), true
	case infoView:
		return m.handleInfoViewKey(msg), true
	}
	return m, false
}

// handleHelpViewKey handles modal scrolling after global keys have had first
// claim.
func (m Model) handleHelpViewKey(msg tea.KeyPressMsg) Model {
	switch {
	case key.Matches(msg, m.keys.Back):
		m = m.goBack()
	case key.Matches(msg, m.keys.Up):
		m = m.scrollHelp(-1)
	case key.Matches(msg, m.keys.Down):
		m = m.scrollHelp(1)
	case key.Matches(msg, m.keys.PageUp):
		m = m.scrollHelp(-m.modalVisible())
	case key.Matches(msg, m.keys.PageDown):
		m = m.scrollHelp(m.modalVisible())
	}
	return m
}

// handleInfoViewKey handles modal scrolling after global keys have had first
// claim.
func (m Model) handleInfoViewKey(msg tea.KeyPressMsg) Model {
	switch {
	case key.Matches(msg, m.keys.Back), key.Matches(msg, m.keys.Info):
		m = m.goBack()
	case key.Matches(msg, m.keys.Up):
		m = m.scrollInfo(-1)
	case key.Matches(msg, m.keys.Down):
		m = m.scrollInfo(1)
	case key.Matches(msg, m.keys.PageUp):
		m = m.scrollInfo(-m.modalVisible())
	case key.Matches(msg, m.keys.PageDown):
		m = m.scrollInfo(m.modalVisible())
	}
	return m
}

func (m Model) handleViewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.view {
	case extractView:
		// Extract owns colliding printable keys, so route it before shared commands.
		// Sync height before a target key can lazily build the file picker.
		m.extract.setHeight(m.height)
		next, cmd, _ := m.extract.handleKey(m.keys, msg)
		m.extract = next
		return m, cmd
	case browseView:
		// Browse owns s and all other non-global keys.
		return m.handleBrowseKey(msg)
	case findVersionsView:
		// Find-versions owns its non-global keys, including r and s.
		return m.handleFindVersionsKey(msg)
	case snapshotDiffView:
		// Snapshot diff owns its filters plus r and s; text input was routed first.
		return m.handleSnapshotDiffKey(msg)
	}

	// Match shared repository commands after views that own the same keys but
	// before list and detail handlers.
	if next, cmd, handled := m.handleRepoCommandKey(msg); handled {
		return next, cmd
	}

	if m.view == detailView {
		return m.handleDetailKey(msg)
	}
	return m.handleListKey(msg)
}

// handleRepoCommandKey handles shared list/detail commands. Its bool reports
// whether the key was consumed.
func (m Model) handleRepoCommandKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.RefreshAll):
		// Refresh-all is independent of the active list or detail cursor.
		m.statusMsg = ""
		wasIdle := len(m.pending) == 0
		var cmds []tea.Cmd
		for _, r := range m.rows {
			if cmd := m.startRefresh(r.Name); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// Start one spinner loop only on the idle-to-refreshing transition.
		if wasIdle && len(cmds) > 0 {
			cmds = append(cmds, m.spinner.Tick)
		}
		return m, tea.Batch(cmds...), true
	case key.Matches(msg, m.keys.Shell):
		// Shell into the selected detail snapshot or repository. Browse handled
		// its own s before reaching this path.
		if cmd := m.openShellCmd(m.shellSnap()); cmd != nil {
			m.statusMsg = ""
			return m, cmd, true
		}
		return m, nil, true
	case key.Matches(msg, m.keys.Refresh):
		if name, ok := m.actionRepo(); ok {
			m.statusMsg = ""
			wasIdle := len(m.pending) == 0
			cmd := m.startRefresh(name)
			// Start the spinner only when transitioning from idle.
			if cmd != nil && wasIdle {
				return m, tea.Batch(cmd, m.spinner.Tick), true
			}
			return m, cmd, true
		}
		return m, nil, true
	}
	return m, nil, false
}

// goBack handles non-canceling routes toward the list. Browse and find-versions
// use their own helpers so backing out cancels the underlying restic process.
func (m Model) goBack() Model {
	switch m.view {
	case detailView:
		// Marks survive browse and diff round trips but not a return to the list.
		// Grouping and collapse are also scoped to one detail visit.
		m = m.clearDetailMarks()
		m.snapGroupMode = snapGroupOff
		m.snapCollapseTree = false
		m.view = listView
	case helpView:
		m.view = m.prevView
	case infoView:
		// Info is reachable only from detail and must preserve detail marks.
		m.view = detailView
	}
	return m
}

// toggleHelp opens the overlay or returns to the view that opened it.
func (m Model) toggleHelp() Model {
	if m.view == helpView {
		m.view = m.prevView
		return m
	}
	m.prevView = m.view
	m.view = helpView
	m.helpScroll = 0 // a fresh open always starts at the top
	return m
}
