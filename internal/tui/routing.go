package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleKey dispatches a keypress. The keys that mean the same thing everywhere
// (the hard quit, help, shell, refresh) are handled first; anything else is
// routed to the active view's handler, where ↑/↓ and enter carry view-specific
// meaning. q is dual-role: it quits from the main list but steps back one screen
// from any nested view, so repeated q walks home and then exits.
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

// handleInputKey gives active text inputs first claim on every key so printable
// globals like q/r/s/? are literal query text until the input is accepted,
// cancelled, or hard-quit.
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

// handleGlobalKey matches keys that are valid from every view, including modal
// overlays. Context-aware q is delegated because nested views need their own
// cancel-aware back paths before the generic goBack path is safe.
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
	m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
	return m
}

// handleQuitKey implements q's dual role: it quits from the main list but steps
// back one screen from nested views, so repeated q walks home and then exits.
func (m Model) handleQuitKey() (tea.Model, tea.Cmd) {
	switch m.view {
	case browseView:
		return m.browseBack(), nil
	case findVersionsView:
		return m.findVersionsBack(), nil
	case snapshotDiffView:
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

// handleHelpViewKey keeps the help overlay modal: only Back closes it. The
// global ctrl+c/q/? path has already had first claim in handleGlobalKey.
func (m Model) handleHelpViewKey(msg tea.KeyPressMsg) Model {
	if key.Matches(msg, m.keys.Back) {
		return m.goBack()
	}
	return m
}

// handleInfoViewKey keeps the info modal modal while still allowing its scroll
// keys. The global ctrl+c/q/? path has already had first claim in handleGlobalKey.
func (m Model) handleInfoViewKey(msg tea.KeyPressMsg) Model {
	switch {
	case key.Matches(msg, m.keys.Back), key.Matches(msg, m.keys.Info):
		m = m.goBack()
	case key.Matches(msg, m.keys.Up):
		m = m.scrollInfo(-1)
	case key.Matches(msg, m.keys.Down):
		m = m.scrollInfo(1)
	case key.Matches(msg, m.keys.PageUp):
		m = m.scrollInfo(-m.infoVisible())
	case key.Matches(msg, m.keys.PageDown):
		m = m.scrollInfo(m.infoVisible())
	}
	return m
}

func (m Model) handleViewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.view {
	case browseView:
		// Browse owns all its non-global keys (including s=shell), so it is routed
		// before the shared refresh/shell handlers below would steal s.
		return m.handleBrowseKey(msg)
	case findVersionsView:
		// Find-versions is its own modal view; it owns all its non-global keys so
		// it must be routed ahead of the shared refresh/shell handlers (which
		// would otherwise steal `r` or `s` on this view).
		return m.handleFindVersionsKey(msg)
	case snapshotDiffView:
		// Snapshot-diff also owns all its non-global keys (including the +/-MUTb
		// filter toggles, which would collide with literal text in the filter/search
		// input paths above). Route here before handleRepoCommandKey so `r`/`s` are
		// not stolen on the diff view.
		return m.handleSnapshotDiffKey(msg)
	}

	// Refresh-all, shell, and per-repo refresh act on repos regardless of view, so
	// they are matched here, after browse (which owns s) but before the list/detail
	// handlers.
	if next, cmd, handled := m.handleRepoCommandKey(msg); handled {
		return next, cmd
	}

	if m.view == detailView {
		return m.handleDetailKey(msg)
	}
	return m.handleListKey(msg)
}

// handleRepoCommandKey handles the keys that launch repo commands regardless of
// view (refresh-all, shell, per-repo refresh) from the list and detail views.
// The trailing bool reports whether the key was consumed; false means handleKey
// should fall through to the view handler.
func (m Model) handleRepoCommandKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.RefreshAll):
		// Refresh-all acts on every repo, so it needs no per-view cursor and works
		// from the list and detail views alike.
		m.statusMsg = ""
		wasIdle := len(m.pending) == 0
		var cmds []tea.Cmd
		for _, r := range m.rows {
			if cmd := m.startRefresh(r.Name); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// Restart the spinner only on the idle->refreshing edge so a refresh-all
		// fired while one is already running doesn't stack a second tick loop.
		if wasIdle && len(cmds) > 0 {
			cmds = append(cmds, m.spinner.Tick)
		}
		return m, tea.Batch(cmds...), true
	case key.Matches(msg, m.keys.Shell):
		// `s` shells into the active scope: a snapshot when one is highlighted
		// (detail view), otherwise the repo. Browse's own `s` handling never
		// reaches this — handleBrowseKey runs first — so shellSnap covers only the
		// list and detail cases.
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
			// On the idle->refreshing edge, (re)start the spinner alongside the
			// refresh; if one was already in flight its tick loop is still running.
			if cmd != nil && wasIdle {
				return m, tea.Batch(cmd, m.spinner.Tick), true
			}
			return m, cmd, true
		}
		return m, nil, true
	}
	return m, nil, false
}

// goBack steps one screen toward the list: the detail view returns to the list
// and the help overlay returns to the view that opened it. No-op on the list.
// Browse and find-versions are intentionally absent: their back paths must run
// through cancel-aware helpers (browseBack / findVersionsBack) to interrupt the
// underlying restic process, so they are routed in handleKey's Quit branch before
// goBack is reached.
func (m Model) goBack() Model {
	switch m.view {
	case detailView:
		// Leaving the detail context for the list: drop the mark FIFO so a new
		// detail visit starts fresh. The diff and browse sub-views take their
		// own back paths to detail and never reach here, so marks survive
		// detail ↔ diff and detail ↔ browse round-trips by construction. Snap
		// grouping and collapse mode are visit-scoped too, so reset both here.
		m = m.clearDetailMarks()
		m.snapGroupMode = snapGroupOff
		m.snapCollapseTree = false
		m.view = listView
	case helpView:
		m.view = m.prevView
	case infoView:
		// info is only reachable from detail, so prevView is unnecessary; the
		// detail arm above is the only one that clears marks, and infoView
		// never reaches it, so the mark FIFO survives the round-trip.
		m.view = detailView
	}
	return m
}

// toggleHelp opens the help overlay from the current view, or closes it back to
// the view it was opened from. Remembering the origin lets `?` from the detail
// view return there rather than dumping the user on the list.
func (m Model) toggleHelp() Model {
	if m.view == helpView {
		m.view = m.prevView
		return m
	}
	m.prevView = m.view
	m.view = helpView
	return m
}
