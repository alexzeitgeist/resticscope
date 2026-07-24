package tui

import (
	"context"
	"errors"

	"github.com/alexzeitgeist/resticscope/internal/app"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Find versions groups one restic query per request by size and modification
// time. Result filenames and snapshot IDs remain in the model only until the
// view closes.

// startFindVersions pins the browse query and starts with its originating host.
// It supersedes prior work before clearing the old cancellation handle.
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

// beginFind supersedes prior work and dispatches a generation-tagged child of
// the program context.
func (m Model) beginFind() (Model, tea.Cmd) {
	m = m.supersedeFind()
	return m.dispatchFind()
}

func (m Model) dispatchFind() (Model, tea.Cmd) {
	fctx, fcancel := context.WithCancel(m.ctx)
	m.findCancel = fcancel
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

// applyFindVersionsMsg drops stale results and installs the latest filter record.
// Errors clear prior rows; unknown hosts offer widening, while other errors show
// their first line.
func (m Model) applyFindVersionsMsg(msg findVersionsMsg) Model {
	if msg.gen != m.findGen {
		return m
	}
	m = m.cancelFind()
	if msg.err != nil {
		m.findRows = nil
		m.findCursor = 0
		m.findResultHost = ""
		m.findResultAllHosts = false
		if errors.Is(msg.err, app.ErrFindUnknownHost) {
			m.findErr = "versions: snapshot host unknown · press a to search all hosts"
			return m
		}
		m.findErr = "versions: " + firstLine(msg.err.Error())
		return m
	}
	m.findErr = ""
	m.findResultHost = msg.result.Host
	m.findResultAllHosts = msg.result.AllHosts
	m.findRows = msg.result.Rows
	m.findCursor = clampCursor(m.findCursor, len(m.findRows))
	return m
}

// handleFindVersionsKey routes navigation, host widening, and extraction. Row
// actions pause while results are pending replacement.
func (m Model) handleFindVersionsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		return m.findVersionsBack(), nil
	case key.Matches(msg, m.keys.HostToggle):
		m.findRequestAllHosts = !m.findRequestAllHosts
		return m.beginFind()
	}

	if m.findLoading() {
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
	case key.Matches(msg, m.keys.Extract), key.Matches(msg, m.keys.Enter):
		// Extract the selected version's newest occurrence through the shared modal.
		m = m.openExtractVersion()
	}
	return m, nil
}

// openExtractVersion opens the selected version's newest occurrence. Setup
// errors remain on the intact versions table.
func (m Model) openExtractVersion() Model {
	if m.findCursor < 0 || m.findCursor >= len(m.findRows) {
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
	sub, err := newExtractModel(m.app, m.ctx, m.seedTargetMemo(req), v.Size)
	if err != nil {
		// Planning errors identify fields without including paths.
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m
	}
	m.statusMsg = ""
	m.extract = sub
	m.extractReturn = findVersionsView
	m.view = extractView
	return m
}

// findVersionsBack supersedes before clearing so a racing result cannot
// repopulate filename-bearing state after return to browse.
func (m Model) findVersionsBack() Model {
	m.statusMsg = ""
	m = m.supersedeFind()
	m.view = browseView
	return m.clearFindVersions()
}

// supersedeFind advances generation and cancels in-flight work.
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

func (m Model) findLoading() bool {
	return m.findCancel != nil
}

// clearFindVersions removes all repository, path, host, and result state on exit.
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
	m.findCancel = nil
	return m
}
