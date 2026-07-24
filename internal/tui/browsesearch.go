package tui

import (
	"path"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleBrowseSearchKey routes search input. Arrow bindings move results while
// j/k/h/l remain query text; accepting jumps to a match, cancelling restores the
// listing, and other printable input reruns the search.
func (m Model) handleBrowseSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.HardQuit):
		return m.quitModel(), tea.Quit
	case key.Matches(msg, m.keys.SearchAccept):
		return m.acceptBrowseSearch()
	case key.Matches(msg, m.keys.SearchCancel):
		return m.cancelBrowseSearch(), nil
	case key.Matches(msg, m.keys.SearchUp):
		if m.browseSearchCursor > 0 {
			m.browseSearchCursor--
		}
		return m, nil
	case key.Matches(msg, m.keys.SearchDown):
		if m.browseSearchCursor < len(m.browseSearchRows)-1 {
			m.browseSearchCursor++
		}
		return m, nil
	case key.Matches(msg, m.keys.PageUp):
		m.browseSearchCursor = clampCursor(m.browseSearchCursor-m.browseVisible(), len(m.browseSearchRows))
		return m, nil
	case key.Matches(msg, m.keys.PageDown):
		m.browseSearchCursor = clampCursor(m.browseSearchCursor+m.browseVisible(), len(m.browseSearchRows))
		return m, nil
	case key.Matches(msg, m.keys.FilterDelete):
		if r := []rune(m.browseSearchQuery); len(r) > 0 {
			m.browseSearchQuery = string(r[:len(r)-1])
			return m.fireBrowseSearch()
		}
		return m, nil
	default:
		// Empty text keeps unhandled control keys out of the query.
		if msg.Text != "" {
			m.browseSearchQuery += msg.Text
			return m.fireBrowseSearch()
		}
		return m, nil
	}
}

// fireBrowseSearch resets the cursor and searches the session store. Empty
// queries clear synchronously; other queries supersede the prior scan without
// marking browse as loading, so existing results remain navigable.
func (m Model) fireBrowseSearch() (Model, tea.Cmd) {
	m.browseSearchCursor = 0
	if strings.TrimSpace(m.browseSearchQuery) == "" {
		m = m.supersedeBrowse()
		m = m.clearBrowseSearchResults()
		m.browseSearchShownQuery = m.browseSearchQuery
		return m, nil
	}

	m, bctx, gen := m.beginBrowseOp()
	repo, snapshotID, query := m.browseRepo, m.browseSnapshot, m.browseSearchQuery
	cmd := func() tea.Msg {
		result, err := m.app.SearchSnapshot(bctx, repo, snapshotID, query, browseSearchResultLimit)
		return browseSearchMsg{gen: gen, query: query, result: result, err: err}
	}
	return m, cmd
}

// applyBrowseSearch installs results only for the open query's current
// generation. Errors remain visible as footer text with rows cleared;
// success installs ranked rows and the uncapped total.
func (m Model) applyBrowseSearch(msg browseSearchMsg) Model {
	if msg.gen != m.browseGen || !m.browseSearching || msg.query != m.browseSearchQuery {
		return m
	}
	// Associate visible rows with their query so Enter cannot select stale rows
	// while an edited query is in flight.
	m.browseSearchShownQuery = msg.query
	if msg.err != nil {
		m = m.clearBrowseSearchResults()
		m.browseSearchErr = "browse search: " + firstLine(msg.err.Error())
		return m
	}
	m.browseSearchErr = ""
	m.browseSearchRows = msg.result.Rows
	m.browseSearchTotal = msg.result.Total
	m.browseSearchCursor = clampCursor(m.browseSearchCursor, len(m.browseSearchRows))
	return m
}

// acceptBrowseSearch opens a match's parent with that match selected. It parks
// search state so escape can restore the results; accepting without a selection
// cancels and clears search.
func (m Model) acceptBrowseSearch() (Model, tea.Cmd) {
	// Ignore Enter while visible rows belong to an older query.
	if m.browseSearchShownQuery != m.browseSearchQuery {
		return m, nil
	}
	e := m.selectedBrowseSearchEntry()
	if e == nil {
		return m.cancelBrowseSearch(), nil
	}
	target := e.Path
	m = m.suspendBrowseSearch()
	return m.beginListDir(path.Dir(target), target)
}

// suspendBrowseSearch parks query, rows, and cursor behind a jumped-to listing.
// Filenames intentionally outlive the overlay but never browse itself because
// clearBrowse clears all search fields.
func (m Model) suspendBrowseSearch() Model {
	m.browseSearching = false
	m.browseSearchSuspended = true
	return m
}

// restoreBrowseSearch reopens the parked query, rows, and cursor. It first
// supersedes an in-flight jump listing so a later generation change cannot drop
// that result while leaving browse permanently marked as loading.
func (m Model) restoreBrowseSearch() Model {
	if m.isBrowseLoading {
		m = m.supersedeBrowse()
		m.isBrowseLoading = false
	}
	m.browseSearching = true
	m.browseSearchSuspended = false
	return m
}

// cancelBrowseSearch preserves the directory listing while cancelling in-flight
// search and clearing its state.
func (m Model) cancelBrowseSearch() Model {
	m = m.supersedeBrowse()
	return m.exitBrowseSearch()
}

// exitBrowseSearch clears active and suspended search state, including all
// filenames, without touching the directory listing. Callers must supersede
// separately when an in-flight scan needs cancellation.
func (m Model) exitBrowseSearch() Model {
	m.browseSearching = false
	m.browseSearchSuspended = false
	m.browseSearchQuery = ""
	m.browseSearchShownQuery = ""
	m.browseSearchCursor = 0
	return m.clearBrowseSearchResults()
}

// clearBrowseSearchResults centralizes clearing every result-bearing field.
func (m Model) clearBrowseSearchResults() Model {
	m.browseSearchRows = nil
	m.browseSearchTotal = 0
	m.browseSearchErr = ""
	return m
}

// selectedBrowseSearchEntry returns the cursor match, or nil when out of range.
func (m Model) selectedBrowseSearchEntry() *model.BrowseEntry {
	if m.browseSearchCursor < 0 || m.browseSearchCursor >= len(m.browseSearchRows) {
		return nil
	}
	return &m.browseSearchRows[m.browseSearchCursor]
}
