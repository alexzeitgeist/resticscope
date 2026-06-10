package tui

import (
	"context"
	"errors"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
)

// findversions.go is the controller for the read-only "show me other versions
// of this file across snapshots" view. One restic call per open (FindMatches),
// grouped by (Size, ModTime) into distinct versions. The result rows hold
// filenames and snapshot ids only for the lifetime of the model — clearFindVersions
// zeroes them on leaving the view, so no path data lingers past the user's exit
// (non-negotiable #1, same discipline as browse rows).

// startFindVersions enters the find-versions view for the file currently
// selected in browse. It pins the (repo, origin host, path) on the model so a
// later `a` toggle re-runs the same logical query, then kicks off the first
// find with the default host filter (the originating snapshot's hostname).
// clearFindVersions is the single source of truth for the find-* zero state;
// supersede first so a prior in-flight find is canceled before clear drops its
// cancel function.
func (m Model) startFindVersions(repo, originHost, p string) (Model, tea.Cmd) {
	m = m.supersedeFind()
	m = m.clearFindVersions()
	m.findRepo = repo
	m.findOriginHost = originHost
	m.findPath = p
	m.statusMsg = ""
	m.view = findVersionsView
	return m.dispatchFind()
}

// beginFind supersedes any prior find (advancing the generation and cancelling
// any in-flight restic call) and dispatches the find. The new context is a
// child of m.ctx so a program-level quit still cascades, but back/toggle can
// cancel just the find. The dispatched Cmd carries the generation so a late
// result from a superseded request is dropped by applyFindVersionsMsg.
func (m Model) beginFind() (Model, tea.Cmd) {
	m = m.supersedeFind()
	return m.dispatchFind()
}

func (m Model) dispatchFind() (Model, tea.Cmd) {
	fctx, fcancel := context.WithCancel(m.ctx)
	m.findCancel = fcancel
	m.findLoading = true
	m.findErr = ""

	gen := m.findGen
	repo, originHost, p, allHosts := m.findRepo, m.findOriginHost, m.findPath, m.findRequestAllHosts
	a := m.app
	cmd := func() tea.Msg {
		res, err := a.FindFileVersions(fctx, repo, originHost, p, allHosts)
		return findVersionsMsg{gen: gen, result: res, err: err}
	}
	return m, cmd
}

// applyFindVersionsMsg installs the result of a find call. A message whose
// generation no longer matches m.findGen is from a superseded or cancelled
// request and is dropped without touching state. On ErrFindUnknownHost the
// recovery affordance (press `a` to widen) is surfaced in the status string.
// On any other error the path-free first line is surfaced. Either error
// clears the result-of-record fields so the header label never reflects the
// prior successful filter while showing the error message for the new one.
// On success the result-of-record fields are written here exactly once per
// response, so the renderer reads what the app actually used.
func (m Model) applyFindVersionsMsg(msg findVersionsMsg) Model {
	if msg.gen != m.findGen {
		return m
	}
	m.findLoading = false
	m = m.cancelFind()
	if msg.err != nil {
		m.findRows = nil
		m.findCursor = 0
		m.findResultHost = ""
		m.findResultAllHosts = false
		if errors.Is(msg.err, app.ErrFindUnknownHost) {
			m.findErr = "find: snapshot host unknown · press a to search all hosts"
			return m
		}
		m.findErr = "find: " + firstLine(msg.err.Error())
		return m
	}
	m.findErr = ""
	m.findResultHost = msg.result.Host
	m.findResultAllHosts = msg.result.AllHosts
	m.findRows = msg.result.Rows
	m.findCursor = clampCursor(m.findCursor, len(m.findRows))
	return m
}

// handleFindVersionsKey routes keys in the find-versions view. Back (esc) and
// the contextual q (handled in handleKey's Quit branch) both return to browse.
// `a` toggles the request-intent flag and re-runs the find, which is also how
// the ErrFindUnknownHost case recovers. Cursor moves are paused while a find
// is loading because the row set is about to be replaced.
func (m Model) handleFindVersionsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		return m.findVersionsBack(), nil
	case key.Matches(msg, m.keys.HostToggle):
		m.findRequestAllHosts = !m.findRequestAllHosts
		return m.beginFind()
	}

	if m.findLoading {
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Up):
		if m.findCursor > 0 {
			m.findCursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.findCursor < len(m.findRows)-1 {
			m.findCursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.findCursor = clampCursor(m.findCursor-m.findVisible(), len(m.findRows))
	case key.Matches(msg, m.keys.PageDown):
		m.findCursor = clampCursor(m.findCursor+m.findVisible(), len(m.findRows))
	case key.Matches(msg, m.keys.Extract):
		// e extracts the queried file from the selected version's newest
		// occurrence snapshot, through the shared extract modal. Sits below the
		// findLoading guard so it can't fire against a row set being replaced.
		m = m.openExtractVersion()
	}
	return m, nil
}

// openExtractVersion launches the extract modal for the version row under the
// cursor, extracting the queried file from the version's newest occurrence —
// the snapshot the Latest column shows, so the output lands under that
// snapshot's mirror dir. Errors surface on the status line and stay in
// find-versions; only a clean construction switches to extractView. The find
// state stays on the model while the modal is open (same as browse), so
// leaving the modal lands back on the intact result table.
func (m Model) openExtractVersion() Model {
	if m.findCursor >= len(m.findRows) {
		return m
	}
	v := m.findRows[m.findCursor]
	if len(v.Occurrences) == 0 {
		return m
	}
	req, err := extractRequestFromFindVersion(m.findRepo, v.Occurrences[0].SnapshotID, m.findPath)
	if err != nil {
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m
	}
	sub, err := newExtractModel(m.app, m.ctx, req, v.Size)
	if err != nil {
		// PlanExtractPaths returns a path-free ErrExtractInvalidRequest naming the
		// offending field, so the notice carries no path either.
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m
	}
	m.statusMsg = ""
	m.extract = sub
	m.extractReturn = findVersionsView
	m.view = extractView
	return m
}

// findVersionsBack leaves the find-versions view and returns to browse with
// the prior browse state intact. The order is load-bearing: bump the
// generation first so any racing findVersionsMsg is rejected on arrival;
// then cancel the in-flight restic call so it does not outlive the user's
// exit; only then switch the view and clear the find-state fields. If the
// clear ran before the gen bump, a racing message could repopulate the just-
// cleared rows.
func (m Model) findVersionsBack() Model {
	m.statusMsg = ""
	m = m.supersedeFind()
	m.view = browseView
	return m.clearFindVersions()
}

// supersedeFind advances the find generation and cancels any in-flight find.
// Mirrors supersedeBrowse — used by every entry point that starts or ends a
// find so a late response cannot resurrect stale state.
func (m Model) supersedeFind() Model {
	m.findGen++
	return m.cancelFind()
}

func (m Model) cancelFind() Model {
	if m.findCancel != nil {
		m.findCancel()
		m.findCancel = nil
	}
	return m
}

// clearFindVersions zeroes all find-versions state. Called on every exit from
// the view so no repo, path, or host string lingers in the model past the
// user's leave (non-negotiable #1: paths do not persist).
func (m Model) clearFindVersions() Model {
	m.findRepo = ""
	m.findOriginHost = ""
	m.findPath = ""
	m.findRequestAllHosts = false
	m.findResultHost = ""
	m.findResultAllHosts = false
	m.findRows = nil
	m.findCursor = 0
	m.findErr = ""
	m.findLoading = false
	m.findCancel = nil
	return m
}
