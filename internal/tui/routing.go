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
	// While typing a filter, every key feeds the query (so "q", "r", etc. are
	// literal text); only apply/clear and ctrl+c escape it.
	if m.filtering {
		return m.handleFilterKey(msg)
	}

	// While the global filename search is open, every key feeds it too (so "q",
	// "s", "?", "h", "l" are literal text or cursor moves, never view actions);
	// only enter/esc/ctrl+c escape it. This guard sits above the global quit/help
	// switch to ensure the search input is fully modal, like the list filter above.
	if m.browseSearching {
		return m.handleBrowseSearchKey(msg)
	}

	// The hard quit, the context-aware q, and the help overlay toggle are matched
	// from every view, including the overlay itself.
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		// Unconditional hard quit from anywhere.
		m.quitting = true
		m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
		return m, tea.Quit
	case key.Matches(msg, m.keys.Quit):
		// q quits only on the main list; on any nested view it steps back one
		// screen like esc, so repeated q walks home and then exits. Browse and
		// find-versions each need a cancel-aware back (so the underlying restic
		// process is interrupted on exit), so they route to their own helpers
		// rather than through the generic goBack.
		if m.view == browseView {
			return m.browseBack(), nil
		}
		if m.view == findVersionsView {
			return m.findVersionsBack(), nil
		}
		if m.view == listView {
			m.quitting = true
			m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
			return m, tea.Quit
		}
		return m.goBack(), nil
	case key.Matches(msg, m.keys.Help):
		return m.toggleHelp(), nil
	}

	// The help overlay is modal: behind it only Back closes the overlay (quit
	// and the help toggle are handled above); action keys do nothing.
	if m.view == helpView {
		if key.Matches(msg, m.keys.Back) {
			m = m.goBack()
		}
		return m, nil
	}

	// Browse owns all its non-global keys (including s=shell), so it is routed
	// before the shared refresh/shell handlers below would steal s.
	if m.view == browseView {
		return m.handleBrowseKey(msg)
	}

	// Find-versions is its own modal view; it owns all its non-global keys so
	// it must be routed ahead of the shared refresh/shell handlers (which
	// would otherwise steal `r` or `s` on this view).
	if m.view == findVersionsView {
		return m.handleFindVersionsKey(msg)
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
		m.view = listView
	case helpView:
		m.view = m.prevView
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
