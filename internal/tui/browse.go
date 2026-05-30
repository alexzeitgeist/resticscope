package tui

import (
	"context"
	"path"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/model"
)

// browse.go is the TUI's in-app snapshot file browser: the controller that
// starts/cancels indexing and directory listings, and the renderer for
// browseView. The first time a snapshot is browsed its whole namespace is
// streamed once into the session-scoped encrypted store (app.Browse); all later
// navigation is a SQL directory query. The on-screen rows (m.browseRows) hold
// filenames only for the lifetime of the model and are cleared on leaving browse;
// the underlying store is encrypted at rest with an ephemeral in-memory key and
// torn down on clean exit. Leaving browse clears the UI rows but NOT the session
// DB, so returning to an already-indexed snapshot in the same run is instant by design.

// browseMetaRows is the number of fixed lines the browse body renders above the
// scrolling entry list (the current-path line and the entry-count/indexing
// summary).
const browseMetaRows = 2

// browseRateWindow is the minimum sample span for the displayed recent indexing
// rate. It avoids the misleading startup-amortized cumulative average while
// keeping the number stable enough to read.
const browseRateWindow = 2 * time.Second

// browseProgressBuffer bounds the index-progress channel. Progress sends are
// non-blocking, so the restic stdout consumer never stalls behind a full UI
// channel: a dropped tick is harmless because a later tick (or the final count)
// carries a fresher number.
const browseProgressBuffer = 64

// browseSearchResultLimit caps how many ranked matches one global filename
// search returns. The store ranks across every prefiltered match and keeps only
// the best this many, so the UI state stays bounded even when a broad query
// matches a large fraction of the snapshot; the footer reports the true total so
// the user knows the list was trimmed.
const browseSearchResultLimit = 200

// startBrowse begins browsing a snapshot. It switches to browseView immediately
// (showing the indexing state with no listing yet) and kicks off the one-time
// index; the first directory listing arrives later, after the index commits.
func (m Model) startBrowse(repo, snapshotID string) (Model, tea.Cmd) {
	m.browseRepo = repo
	m.browseSnapshot = snapshotID
	m.browseDir = "/"
	m.browseRows = nil
	m.browseCache = nil // a fresh snapshot: never serve a previous one's cached dirs
	m.browseCursor = 0
	m.browseSortMode = browseSortName // a prior session's sort must not leak in
	m.view = browseView
	// beginIndex owns the index-counter reset (browseIndexed, browseIndexN, and
	// the rate fields), so startBrowse only sets the navigation state here.
	return m.beginIndex()
}

// beginIndex sets up the generation/cancel/progress for the one-time index and
// returns the commands that run it. It advances the generation token (so any
// superseded run's late messages are discarded), cancels any prior in-flight
// browse, and derives a fresh cancel from m.ctx — a child of the program context
// so quitting still cascades, but back can cancel just the browse. The index runs
// in one Cmd while a second Cmd pumps progress ticks; both carry the generation.
func (m Model) beginIndex() (Model, tea.Cmd) {
	m, bctx, gen := m.beginBrowseOp()
	m.browseLoading = true
	m.browseIndexed = false
	m.browseIndexN = 0
	m.browseIndexRate = 0
	m.browseRateBaseN = 0
	m.browseRateBaseAt = time.Time{}

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
		close(progress) // ends the paired waitForIndexProgress Cmd
		return browseIndexedMsg{gen: gen, err: err}
	}
	return m, tea.Batch(indexCmd, waitForIndexProgress(gen, progress))
}

// waitForIndexProgress blocks on the progress channel and turns the next tick
// into a browseIndexProgressMsg, re-arming itself in Update so the count climbs
// live. A closed channel (the index command finished) returns a nil msg, which
// ends the loop.
func waitForIndexProgress(gen int, progress <-chan int) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-progress
		if !ok {
			return nil
		}
		return browseIndexProgressMsg{gen: gen, n: n}
	}
}

// applyBrowseIndexProgress records a progress tick and re-arms the wait command.
// A tick whose generation no longer matches is from a superseded index and is
// dropped without re-arming (the superseding run owns its own channel). The count
// is clamped monotonic so out-of-order ticks never make it jump backwards.
func (m Model) applyBrowseIndexProgress(msg browseIndexProgressMsg) (Model, tea.Cmd) {
	if msg.gen != m.browseGen {
		return m, nil
	}
	if msg.n > m.browseIndexN {
		now := time.Now()
		m.browseIndexN = msg.n
		m = m.updateBrowseIndexRate(msg.n, now)
	}
	return m, waitForIndexProgress(msg.gen, m.browseProgress)
}

func (m Model) updateBrowseIndexRate(n int, now time.Time) Model {
	if m.browseRateBaseAt.IsZero() {
		m.browseRateBaseN = n
		m.browseRateBaseAt = now
		return m
	}
	elapsed := now.Sub(m.browseRateBaseAt)
	if elapsed < browseRateWindow || n <= m.browseRateBaseN {
		return m
	}
	m.browseIndexRate = float64(n-m.browseRateBaseN) / elapsed.Seconds()
	m.browseRateBaseN = n
	m.browseRateBaseAt = now
	return m
}

// applyBrowseIndexed handles a finished one-time index. A result whose generation
// no longer matches is from a cancelled or superseded run and is dropped. On an
// error there is no listing to show: the already-redacted restic/store message is
// surfaced in the status line and we fall back to the detail view. Secrets are
// redacted, but a restic-printed filesystem path can appear here transiently (by
// design; the scoped shell is the path-free alternative); it is shown on screen
// only and never persisted. On success the first directory is listed.
func (m Model) applyBrowseIndexed(msg browseIndexedMsg) (Model, tea.Cmd) {
	if msg.gen != m.browseGen {
		return m, nil
	}
	if msg.err != nil {
		m = m.supersedeBrowse()
		m.browseLoading = false
		m.statusMsg = "browse: " + firstLine(msg.err.Error())
		m.view = detailView
		return m.clearBrowse(), nil
	}
	m.browseIndexed = true
	return m.beginListDir("/", "")
}

// beginListDir kicks off a directory-listing query against the session store.
// selectPath is the child the cursor should land on when the rows arrive (empty
// for the top of the list). Like beginIndex it advances the generation and
// supersedes any prior in-flight browse so a stale listing can never overwrite a
// newer one.
func (m Model) beginListDir(dir, selectPath string) (Model, tea.Cmd) {
	// Serve an already-visited directory straight from the session listing cache.
	// The snapshot is immutable once indexed, so a cached listing can never go
	// stale; answering synchronously skips the async query and its loading hop, so
	// rapid parent/back navigation stays crisp — handleBrowseKey pauses navigation
	// while browseLoading, which would otherwise drop keystrokes during each query
	// round-trip. supersedeBrowse advances the generation so any in-flight listing's
	// late message is dropped; no new query is dispatched.
	if rows, ok := m.browseCache[dir]; ok {
		m = m.supersedeBrowse()
		m.browseLoading = false
		m.browseDir = dir
		m.browseRows = sortedBrowseRows(rows, m.browseSortMode)
		m.browseCursor = m.indexOfBrowsePath(selectPath)
		return m, nil
	}

	m, bctx, gen := m.beginBrowseOp()
	m.browseLoading = true

	repo, snapshotID := m.browseRepo, m.browseSnapshot
	cmd := func() tea.Msg {
		rows, err := m.app.ListDir(bctx, repo, snapshotID, dir)
		return browseDirMsg{gen: gen, dir: dir, selectPath: selectPath, rows: rows, err: err}
	}
	return m, cmd
}

// applyBrowseDir installs a finished directory listing. A listing whose
// generation no longer matches is from a superseded navigation and is dropped. On
// error the path-free message is surfaced and the current rows are kept. On
// success the rows replace the listing and the cursor lands on selectPath.
func (m Model) applyBrowseDir(msg browseDirMsg) Model {
	if msg.gen != m.browseGen {
		return m
	}
	m.browseLoading = false
	if msg.err != nil {
		m.statusMsg = "browse: " + firstLine(msg.err.Error())
		return m
	}
	m.browseDir = msg.dir
	// Memoize the canonical listing so a later return to this directory is served
	// synchronously (see beginListDir). Because the snapshot is immutable, the entry
	// never needs invalidation; clearBrowse drops the whole map on leaving browse.
	if m.browseCache == nil {
		m.browseCache = make(map[string][]model.BrowseEntry)
	}
	m.browseCache[msg.dir] = msg.rows
	// Display rows derive from the canonical rows under the active sort; for
	// non-name modes sortedBrowseRows copies, so the cached slice stays canonical.
	m.browseRows = sortedBrowseRows(msg.rows, m.browseSortMode)
	m.browseCursor = m.indexOfBrowsePath(msg.selectPath)
	return m
}

// browseBack leaves the browse view, shared by q and esc. It advances the
// generation and cancels any in-flight index/listing (so a cancelled index rolls
// back and its late messages are dropped), returns to the detail view, and clears
// all session browse UI state so no filenames linger. The session store stays
// open: an already-indexed snapshot reopened in the same run skips restic.
func (m Model) browseBack() Model {
	// Drop any transient browse status so a restic/store message never lingers
	// into the view we return to.
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

// clearBrowse drops all session browse UI state. It is called on every exit from
// browse so no filename or path data lingers in the model. It deliberately does
// NOT touch app.Browse — the encrypted session DB stays open until the app exits,
// so returning to the same snapshot in this run does not re-index.
func (m Model) clearBrowse() Model {
	m.browseRows = nil
	m.browseCache = nil // filenames must not linger in the model after leaving browse
	m.browseRepo = ""
	m.browseSnapshot = ""
	m.browseDir = ""
	m.browseCursor = 0
	m.browseSortMode = browseSortName // no sort state survives leaving browse
	m.browseIndexed = false
	m.browseIndexN = 0
	m.browseIndexRate = 0
	m.browseRateBaseN = 0
	m.browseRateBaseAt = time.Time{}
	m.browseLoading = false
	m.browseCancel = nil
	m.browseProgress = nil
	// The search overlay carries filenames/paths too and must be zeroed here on
	// leaving browse (non-negotiable #1: no filenames linger in the model) — including
	// a parked (suspended) result set, the one place search state outlives the overlay.
	m = m.exitBrowseSearch()
	return m
}

// handleBrowseKey routes keys while browsing. Back leaves (cancelling any
// in-flight work); s shells into the browsed snapshot. Navigation is paused while
// an index or listing is in flight, since the cursor would point into rows that
// are about to be replaced.
func (m Model) handleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		// esc is dual-role in browse: while a search result set is parked (the user
		// jumped to a match with Enter) it restores that search overlay rather than
		// leaving browse, to ensure the modal stack pops one level at a time. q still leaves
		// browse outright (handleKey's Quit case → browseBack), the quick escape hatch.
		if m.browseSearchSuspended {
			return m.restoreBrowseSearch(), nil
		}
		return m.browseBack(), nil
	case key.Matches(msg, m.keys.Shell):
		if cmd := m.openShellCmd(m.browseSnapshotPtr()); cmd != nil {
			m.statusMsg = ""
			return m, cmd
		}
		return m, nil
	}

	if m.browseLoading {
		return m, nil // navigation is paused while indexing or a listing is in flight
	}

	switch {
	case key.Matches(msg, m.keys.Up):
		if m.browseCursor > 0 {
			m.browseCursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.browseCursor < m.browseRowCount()-1 {
			m.browseCursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.browseCursor = clampCursor(m.browseCursor-m.browseVisible(), m.browseRowCount())
	case key.Matches(msg, m.keys.PageDown):
		m.browseCursor = clampCursor(m.browseCursor+m.browseVisible(), m.browseRowCount())
	case key.Matches(msg, m.keys.Enter), key.Matches(msg, m.keys.Open):
		return m.openBrowseDir()
	case key.Matches(msg, m.keys.Parent):
		return m.browseToParent()
	case key.Matches(msg, m.keys.Sort):
		// Cycle the display sort of the current directory listing. It sits in the
		// idle-only switch (below the browseLoading guard) so it can't fire
		// mid-index/mid-load. While the search input is open handleKey routes to
		// handleBrowseSearchKey first, so `o` is literal query text there; while a
		// search is suspended it sorts only the visible directory listing.
		return m.cycleBrowseSort(), nil
	case key.Matches(msg, m.keys.Search):
		// `/` opens the global filename search, but only once the snapshot is
		// indexed — there is nothing to search before the one-time crawl commits.
		// It sits in the idle-only switch (below the browseLoading guard) so it
		// can't fire mid-index.
		if m.browseIndexed {
			// Start from a fully cleared overlay (also discards any parked result
			// set), then open the input.
			m = m.exitBrowseSearch()
			m.browseSearching = true
		}
		return m, nil
	}
	return m, nil
}

// cycleBrowseSort advances the browse display sort (name → size → modified → name)
// and keeps the cursor on the same entry across the reorder, mirroring the list
// view's cycleSort. It re-derives the listing from the canonical cached rows so
// cycling back to name restores canonical order rather than re-sorting an
// already-permuted slice. The current dir is cached whenever browse is idle
// (applyBrowseDir caches every successful load; the loading guard blocks `o` until
// then; even the error path leaves browseDir on the last cached dir). The ok guard
// makes that explicit and turns any future violation into a safe no-op instead of
// blanking the listing.
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
// have no action here (the shell is the way to read file contents). It lists the
// directory fresh from the store, cursor at the top.
func (m Model) openBrowseDir() (Model, tea.Cmd) {
	e := m.selectedBrowseEntry()
	if e == nil || !e.IsDir {
		return m, nil
	}
	return m.beginListDir(e.Path, "")
}

// browseToParent steps up one directory, asking the listing to restore the cursor
// onto the child we came from so repeated enter/backspace feels like walking a
// path.
func (m Model) browseToParent() (Model, tea.Cmd) {
	if m.browseDir == "" || m.browseDir == "/" {
		return m, nil
	}
	from := m.browseDir
	return m.beginListDir(path.Dir(m.browseDir), from)
}

// browseSnapshotPtr resolves the browsed snapshot's full record from the cached
// rows so the shell can scope to it. It returns nil if the snapshot is no longer
// present, in which case openShellCmd falls back to a repo-only shell.
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

// browseRowCount is the number of selectable entries in the current directory.
func (m Model) browseRowCount() int {
	return len(m.browseRows)
}

// selectedBrowseEntry returns the entry under the cursor, or nil when the cursor
// is out of range (e.g. an empty directory).
func (m Model) selectedBrowseEntry() *model.BrowseEntry {
	if m.browseCursor < 0 || m.browseCursor >= len(m.browseRows) {
		return nil
	}
	return &m.browseRows[m.browseCursor]
}

// indexOfBrowsePath returns the position of path p among the current rows, or 0
// when it is absent or empty (so the cursor lands at the top).
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
