package tui

import (
	"context"
	"errors"
	"path"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Snapshot browsing indexes a repository namespace into the encrypted session
// store once. UI rows are cleared on exit, while the store remains available
// for immediate return to an indexed snapshot.

// browseMetaRows counts fixed body lines above the scrolling entries.
const browseMetaRows = 2

// browseProgressBuffer bounds non-blocking progress delivery; stale dropped
// ticks are superseded by later ticks or the final count.
const browseProgressBuffer = 64

// browseSearchResultLimit bounds UI state while the footer reports the uncapped
// match count.
const browseSearchResultLimit = 200

// startBrowse enters the indexing view immediately and loads the root listing
// after the one-time index commits.
func (m Model) startBrowse(repo, snapshotID string) (Model, tea.Cmd) {
	m.browseRepo = repo
	m.browseSnapshot = snapshotID
	m.browseDir = "/"
	m.browseRows = nil
	m.browseCache = nil // Never serve cached directories from another snapshot.
	m.browseCursor = 0
	m.browseSortMode = browseSortName
	m.browseNotice = ""
	m.view = browseView
	return m.beginIndex()
}

// beginIndex starts the one-time index and its progress pump. Generation and
// cancellation isolate superseded runs; quitting cancels through m.ctx while
// leaving browse cancels only this operation.
func (m Model) beginIndex() (Model, tea.Cmd) {
	m, bctx, gen := m.beginBrowseOp()
	m.isBrowseLoading = true
	m.browseIndexed = false
	m.browseIndexN = 0
	m.browseRate = rateSampler{}

	repo, snapshotID := m.browseRepo, m.browseSnapshot
	progress := make(chan int, browseProgressBuffer)
	m.browseProgress = progress

	indexCmd := func() tea.Msg {
		err := m.app.IndexSnapshot(bctx, repo, snapshotID, func(n int) {
			select {
			case progress <- n:
			default:
			}
		})
		close(progress)
		return browseIndexedMsg{gen: gen, err: err}
	}
	return m, tea.Batch(indexCmd, waitForIndexProgress(gen, progress))
}

// waitForIndexProgress emits one tick at a time for Update to re-arm. Closing
// progress ends the loop with a nil message.
func waitForIndexProgress(gen int, progress <-chan int) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-progress
		if !ok {
			return nil
		}
		return browseIndexProgressMsg{gen: gen, n: n}
	}
}

// applyBrowseIndexProgress records monotonic ticks and re-arms the wait. It
// drops superseded generations without re-arming them.
func (m Model) applyBrowseIndexProgress(msg browseIndexProgressMsg) (Model, tea.Cmd) {
	if msg.gen != m.browseGen {
		return m, nil
	}
	if msg.n > m.browseIndexN {
		m.browseIndexN = msg.n
		m.browseRate.update(int64(msg.n), time.Now())
	}
	return m, waitForIndexProgress(msg.gen, m.browseProgress)
}

// applyBrowseIndexed drops superseded results and loads the root on success. On
// failure it returns to detail with a redacted, transient error; restic paths may
// appear on screen but are never persisted.
func (m Model) applyBrowseIndexed(msg browseIndexedMsg) (Model, tea.Cmd) {
	if msg.gen != m.browseGen {
		return m, nil
	}
	if msg.err != nil {
		m = m.supersedeBrowse()
		m.isBrowseLoading = false
		m.statusMsg = "browse: " + firstLine(msg.err.Error())
		m.view = detailView
		return m.clearBrowse(), nil
	}
	m.browseIndexed = true
	return m.beginListDir("/", "")
}

// beginListDir loads dir from the session store and selects selectPath when it
// arrives. Generation and cancellation prevent stale listings from replacing
// newer navigation.
func (m Model) beginListDir(dir, selectPath string) (Model, tea.Cmd) {
	m.browseNotice = ""
	// Indexed snapshots are immutable, so cached listings can serve back
	// navigation synchronously. Superseding also rejects any in-flight result.
	if rows, ok := m.browseCache[dir]; ok {
		m = m.supersedeBrowse()
		m.isBrowseLoading = false
		m.browseDir = dir
		m.browseRows = sortedBrowseRows(rows, m.browseSortMode)
		m.browseCursor = m.indexOfBrowsePath(selectPath)
		return m, nil
	}

	m, bctx, gen := m.beginBrowseOp()
	m.isBrowseLoading = true

	repo, snapshotID := m.browseRepo, m.browseSnapshot
	cmd := func() tea.Msg {
		rows, err := m.app.ListDir(bctx, repo, snapshotID, dir)
		return browseDirMsg{gen: gen, dir: dir, selectPath: selectPath, rows: rows, err: err}
	}
	return m, cmd
}

// applyBrowseDir installs a current-generation listing and selects its requested
// path. Errors are path-free and preserve the current rows.
func (m Model) applyBrowseDir(msg browseDirMsg) Model {
	if msg.gen != m.browseGen {
		return m
	}
	m.isBrowseLoading = false
	if msg.err != nil {
		m.statusMsg = "browse: " + firstLine(msg.err.Error())
		return m
	}
	m.browseDir = msg.dir
	// Cache the immutable canonical listing for synchronous return navigation;
	// clearBrowse drops the map on exit.
	if m.browseCache == nil {
		m.browseCache = make(map[string][]model.BrowseEntry)
	}
	m.browseCache[msg.dir] = msg.rows
	// Non-name display sorting copies, preserving canonical cache order.
	m.browseRows = sortedBrowseRows(msg.rows, m.browseSortMode)
	m.browseCursor = m.indexOfBrowsePath(msg.selectPath)
	return m
}

// browseBack cancels in-flight work, clears filename-bearing UI state, and
// returns to detail. The encrypted session store stays open so revisiting an
// indexed snapshot does not invoke restic again.
func (m Model) browseBack() Model {
	// Do not leak transient restic or store messages into the detail view.
	m.statusMsg = ""
	m = m.supersedeBrowse()
	m.view = detailView
	return m.clearBrowse()
}

func (m Model) beginBrowseOp() (Model, context.Context, int) {
	m = m.supersedeBrowse()
	bctx, bcancel := context.WithCancel(m.ctx)
	m.browseCancel = bcancel
	return m, bctx, m.browseGen
}

func (m Model) supersedeBrowse() Model {
	m.browseGen++
	return m.cancelBrowse()
}

func (m Model) cancelBrowse() Model {
	if m.browseCancel != nil {
		m.browseCancel()
		m.browseCancel = nil
	}
	return m
}

// clearBrowse removes browse navigation and search data when a browse session
// ends. It leaves app.Browse's encrypted session database open until process
// exit so the same snapshot need not be re-indexed.
func (m Model) clearBrowse() Model {
	m.browseRows = nil
	m.browseCache = nil
	m.browseRepo = ""
	m.browseSnapshot = ""
	m.browseDir = ""
	m.browseCursor = 0
	m.browseSortMode = browseSortName
	m.browseIndexed = false
	m.browseIndexN = 0
	m.browseRate = rateSampler{}
	m.isBrowseLoading = false
	m.browseNotice = ""
	m.browseCancel = nil
	m.browseProgress = nil
	// Also clear any suspended search result set carrying filenames or paths.
	m = m.exitBrowseSearch()
	return m
}

// handleBrowseKey routes browse input. Navigation pauses during loads because
// the cursor would otherwise address rows about to be replaced.
func (m Model) handleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := m.handleBrowseImmediateKey(msg); handled {
		return next, cmd
	}
	if m.isBrowseLoading {
		return m, nil
	}
	return m.handleIdleBrowseKey(msg)
}

func (m Model) handleBrowseImmediateKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Back):
		// Escape restores a suspended search before leaving browse, while q leaves
		// browse directly, so the modal stack unwinds one level at a time.
		if m.browseSearchSuspended {
			return m.restoreBrowseSearch(), nil, true
		}
		return m.browseBack(), nil, true
	case key.Matches(msg, m.keys.Shell):
		m.browseNotice = ""
		if cmd := m.openShellCmd(m.browseSnapshotPtr()); cmd != nil {
			m.statusMsg = ""
			return m, cmd, true
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m Model) handleIdleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := m.handleBrowseNavigationKey(msg); handled {
		return next, cmd
	}
	if next, cmd, handled := m.handleBrowseActionKey(msg); handled {
		return next, cmd
	}
	return m, nil
}

func (m Model) handleBrowseNavigationKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Up):
		m.browseNotice = ""
		if m.browseCursor > 0 {
			m.browseCursor--
		}
		return m, nil, true
	case key.Matches(msg, m.keys.Down):
		m.browseNotice = ""
		if m.browseCursor < m.browseRowCount()-1 {
			m.browseCursor++
		}
		return m, nil, true
	case key.Matches(msg, m.keys.PageUp):
		m.browseNotice = ""
		m.browseCursor = clampCursor(m.browseCursor-m.browseVisible(), m.browseRowCount())
		return m, nil, true
	case key.Matches(msg, m.keys.PageDown):
		m.browseNotice = ""
		m.browseCursor = clampCursor(m.browseCursor+m.browseVisible(), m.browseRowCount())
		return m, nil, true
	case key.Matches(msg, m.keys.Enter), key.Matches(msg, m.keys.Open):
		m.browseNotice = ""
		next, cmd := m.openBrowseDir()
		return next, cmd, true
	case key.Matches(msg, m.keys.Parent):
		m.browseNotice = ""
		next, cmd := m.browseToParent()
		return next, cmd, true
	}
	return m, nil, false
}

func (m Model) handleBrowseActionKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Sort):
		// Search input consumes `o` as text; suspended search sorts only the visible
		// directory listing.
		m.browseNotice = ""
		return m.cycleBrowseSort(), nil, true
	case key.Matches(msg, m.keys.Search):
		// Search is available only after the snapshot index commits.
		return m.openBrowseSearchInput(), nil, true
	case key.Matches(msg, m.keys.Versions):
		next, cmd := m.openBrowseVersions()
		return next, cmd, true
	case key.Matches(msg, m.keys.Extract):
		next, cmd := m.openExtract()
		return next, cmd, true
	}
	return m, nil, false
}

// openExtract opens extraction for a selected regular file or directory. It
// rejects special nodes and setup failures with path-free notices. Quitting
// cancels through m.ctx; leaving the modal cancels only extraction.
func (m Model) openExtract() (Model, tea.Cmd) {
	e := m.selectedBrowseEntry()
	if e == nil {
		return m, nil
	}
	req, err := extractRequestFromBrowseEntry(m.browseRepo, m.browseSnapshot, *e)
	if err != nil {
		if errors.Is(err, ErrExtractUnsupportedType) {
			m.browseNotice = "extract: this entry type is not supported (extract the parent directory instead)"
		} else {
			// Request-shape errors name fields, never source paths.
			m.browseNotice = "extract: " + firstLine(err.Error())
		}
		return m, nil
	}
	sub, err := newExtractModel(m.app, m.ctx, m.seedTargetMemo(req), e.Size)
	if err != nil {
		// Planning errors name the invalid field without including its path.
		m.browseNotice = "extract: " + firstLine(err.Error())
		return m, nil
	}
	m.browseNotice = ""
	m.extract = sub
	m.extractReturn = browseView
	m.view = extractView
	return m, m.extract.countsCmd()
}

func (m Model) openBrowseSearchInput() Model {
	m.browseNotice = ""
	if !m.browseIndexed {
		return m
	}
	// Discard any parked result set before reopening search.
	m = m.exitBrowseSearch()
	m.browseSearching = true
	return m
}

// openBrowseVersions opens versions only for regular files, preserving browse
// state for return. Other node types remain in browse with a notice and cannot
// reach extraction that assumes a regular file.
func (m Model) openBrowseVersions() (Model, tea.Cmd) {
	e := m.selectedBrowseEntry()
	if e == nil {
		return m, nil
	}
	if e.Type != model.NodeTypeFile {
		m.browseNotice = "versions: select a regular file"
		return m, nil
	}
	m.browseNotice = ""
	originHost := ""
	if snap := m.browseSnapshotPtr(); snap != nil {
		originHost = snap.Hostname
	}
	return m.startFindVersions(m.browseRepo, originHost, e.Path)
}

// cycleBrowseSort advances name, size, and modified order while retaining the
// selected entry. Each order derives from canonical cached rows; a missing cache
// safely leaves the listing unchanged.
func (m Model) cycleBrowseSort() Model {
	rows, ok := m.browseCache[m.browseDir]
	if !ok {
		return m
	}
	sel := ""
	if e := m.selectedBrowseEntry(); e != nil {
		sel = e.Path
	}
	m.browseSortMode = (m.browseSortMode + 1) % browseSortModeCount
	m.browseRows = sortedBrowseRows(rows, m.browseSortMode)
	m.browseCursor = m.indexOfBrowsePath(sel)
	return m
}

// openBrowseDir descends into the selected entry when it is a directory; files
// have no action here. It lists the directory fresh from the store, cursor at top.
func (m Model) openBrowseDir() (Model, tea.Cmd) {
	e := m.selectedBrowseEntry()
	if e == nil || !e.IsDir {
		return m, nil
	}
	return m.beginListDir(e.Path, "")
}

// browseToParent moves up one directory and restores the cursor to the child
// just left.
func (m Model) browseToParent() (Model, tea.Cmd) {
	if m.browseDir == "" || m.browseDir == "/" {
		return m, nil
	}
	from := m.browseDir
	return m.beginListDir(path.Dir(m.browseDir), from)
}

// browseSnapshotPtr resolves the cached snapshot for shell scoping. It returns
// nil when absent, allowing openShellCmd to use repository-only scope.
func (m Model) browseSnapshotPtr() *model.Snapshot {
	for _, r := range m.rows {
		if r.Name != m.browseRepo {
			continue
		}
		for i := range r.State.Snapshots {
			if r.State.Snapshots[i].ID == m.browseSnapshot {
				return &r.State.Snapshots[i]
			}
		}
	}
	return nil
}

func (m Model) browseRowCount() int {
	return len(m.browseRows)
}

// selectedBrowseEntry returns the cursor entry, or nil when out of range.
func (m Model) selectedBrowseEntry() *model.BrowseEntry {
	if m.browseCursor < 0 || m.browseCursor >= len(m.browseRows) {
		return nil
	}
	return &m.browseRows[m.browseCursor]
}

// indexOfBrowsePath returns p's row index, or zero when absent or empty.
func (m Model) indexOfBrowsePath(p string) int {
	if p == "" {
		return 0
	}
	for i := range m.browseRows {
		if m.browseRows[i].Path == p {
			return i
		}
	}
	return 0
}
