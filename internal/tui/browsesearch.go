package tui

import (
	"path"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleBrowseSearchKey consumes keys while the global filename search input is
// open. It mirrors handleFilterKey, but the arrows (plus ctrl+k/ctrl+j) move the
// result cursor instead of editing text. Result navigation deliberately avoids the
// plain Up/Down letters (j/k) and there is no Parent/Open binding, so j/k/h/l stay
// literal query characters (json, java, kernel, …). HardQuit is matched first
// because the m.browseSearching guard in handleKey sits above the global quit.
// Accept jumps to the selected match; cancel restores the prior listing untouched;
// every other printable key edits the query and refires the live search.
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
		// Text is non-empty only for printable keys, so this ignores stray control
		// keys (arrows handled above, etc.) rather than inserting garbage.
		if msg.Text != "" {
			m.browseSearchQuery += msg.Text
			return m.fireBrowseSearch()
		}
		return m, nil
	}
}

// fireBrowseSearch runs the current query against the session store. The cursor
// resets to the top on every query change so it never points past the matches. An
// empty/whitespace-only query has nothing to scan: it supersedes any in-flight
// search and clears the results synchronously, with no command. Otherwise it
// supersedes the prior keystroke's scan (advancing the generation and cancelling
// its context so the single connection frees immediately) and dispatches a
// gen-tagged search. It deliberately does NOT set isBrowseLoading: the search list
// must stay live and navigable as results arrive, never paused like a directory
// load. Ranking and the result cap happen in the store, never here.
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

// applyBrowseSearch installs one search result. It is dropped unless it matches
// the current generation, search is still open, and the query is exactly the one
// the model now holds — so a superseded keystroke's late result, or one the user
// has since edited past, never overwrites fresher state. On error the path-free
// store message is shown in the footer and the rows are cleared, but search stays
// open so the error is visible. On success the ranked rows and true total replace
// the prior result and the cursor is clamped into range.
func (m Model) applyBrowseSearch(msg browseSearchMsg) Model {
	if msg.gen != m.browseGen || !m.browseSearching || msg.query != m.browseSearchQuery {
		return m
	}
	// Record the query these rows belong to in both branches: acceptBrowseSearch
	// gates on it so Enter can never act on rows from a query the user has since
	// edited past (the search list stays visible between keystrokes by design, so
	// without this gate a stale row would remain selectable mid-edit).
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

// acceptBrowseSearch jumps to the selected match in its own folder. It SUSPENDS
// the search rather than exiting it (suspendBrowseSearch keeps the query/rows/cursor
// parked), closes the input, and lists the match's parent directory with the file
// preselected — a cache hit serves it instantly, and indexOfBrowsePath lands the
// cursor on the file. esc from that listing restores the parked search so the user
// can pick another match (restoreBrowseSearch). With no selection (empty results) it
// just cancels, fully clearing search.
func (m Model) acceptBrowseSearch() (Model, tea.Cmd) {
	// The visible rows are kept between keystrokes (no per-edit flicker), so an
	// Enter pressed after editing the query but before the new scan returns would
	// otherwise act on the previous query's rows. Gate on the query that actually
	// produced the visible rows: while it lags the live query, Enter is a no-op.
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

// suspendBrowseSearch parks the open search: it closes the input but KEEPS the
// query, ranked rows, and cursor so esc can later restore them (restoreBrowseSearch).
// This is the deliberate relaxation of "no filenames linger after leaving search" —
// Enter suspends rather than exits, so the result set stays in the model while the
// user inspects the jumped-to directory. The broader invariant still holds: leaving
// browse (q/back) calls clearBrowse, which wipes every search field, so no filename
// lingers once the user leaves browse.
func (m Model) suspendBrowseSearch() Model {
	m.browseSearching = false
	m.browseSearchSuspended = true
	return m
}

// restoreBrowseSearch reopens the search overlay that Enter suspended, bringing back
// the previous query, ranked rows, and cursor so the user can pick another match. The
// directory listing underneath is wherever they navigated to; cancelling the restored
// search returns there. It is the esc action while a suspended search is parked (q
// still leaves browse entirely via browseBack).
//
// If the jump listing acceptBrowseSearch started is still in flight (isBrowseLoading),
// drop it first: re-entering search and then superseding it (a fresh query, or cancel)
// would advance the generation past that listing's tag, so its browseDirMsg is dropped
// before applyBrowseDir clears isBrowseLoading — leaving navigation paused forever. The
// user pressed esc to return to the results, so the half-finished jump is moot anyway.
func (m Model) restoreBrowseSearch() Model {
	if m.isBrowseLoading {
		m = m.supersedeBrowse()
		m.isBrowseLoading = false
	}
	m.browseSearching = true
	m.browseSearchSuspended = false
	return m
}

// cancelBrowseSearch closes the search with no navigation, leaving the underlying
// directory listing exactly as it was. It supersedes any in-flight search (so a
// late result is dropped and its scan is cancelled) and then clears only the
// search state.
func (m Model) cancelBrowseSearch() Model {
	m = m.supersedeBrowse()
	return m.exitBrowseSearch()
}

// exitBrowseSearch clears only the search overlay state, leaving the directory
// listing (browseRows/browseDir/browseCursor) untouched so Esc returns the user
// exactly where they were. It also drops any parked (suspended) result set. It does
// not bump the generation or cancel anything — callers that need to drop an in-flight
// scan supersede first. The filenames the search held are zeroed here so none linger
// past the overlay.
func (m Model) exitBrowseSearch() Model {
	m.browseSearching = false
	m.browseSearchSuspended = false
	m.browseSearchQuery = ""
	m.browseSearchShownQuery = ""
	m.browseSearchCursor = 0
	return m.clearBrowseSearchResults()
}

// clearBrowseSearchResults zeros the result-bearing search fields — the ranked
// rows, the match total, and the path-free error. It is shared by the
// empty-query reset, the store-error branch, and the full overlay teardown so a
// future result field can't leak by being cleared in only some of them.
func (m Model) clearBrowseSearchResults() Model {
	m.browseSearchRows = nil
	m.browseSearchTotal = 0
	m.browseSearchErr = ""
	return m
}

// selectedBrowseSearchEntry returns the match under the search cursor, or nil when
// the cursor is out of range (e.g. no matches yet).
func (m Model) selectedBrowseSearchEntry() *model.BrowseEntry {
	if m.browseSearchCursor < 0 || m.browseSearchCursor >= len(m.browseSearchRows) {
		return nil
	}
	return &m.browseSearchRows[m.browseSearchCursor]
}
