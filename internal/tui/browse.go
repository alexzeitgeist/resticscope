package tui

import (
	"context"
	"fmt"
	"path"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// browse.go is the TUI's in-app snapshot file browser: the controller that
// starts/cancels browse loads and the renderer for browseView. Everything here
// is session-only — the tree it holds (m.browseResult) is never persisted to the
// cache, RepoState, or any log, and every exit from browse clears it. A browse
// load runs a single streamed `restic ls --recursive` via app.BrowseSnapshot;
// load-more rebuilds from scratch with doubled caps rather than appending.

// browseMetaRows is the number of fixed lines the browse body renders above the
// scrolling entry list (the current-path line and the loaded/retained summary).
const browseMetaRows = 2

// browseLimits converts the configured Browse block into the model's BrowseLimits
// (the typed config units become plain ints/int64/Duration the model works in).
func browseLimits(b config.Browse) model.BrowseLimits {
	return model.BrowseLimits{
		MaxEntries:          b.MaxEntries,
		MaxJSONBytes:        b.MaxJSONBytes.Bytes(),
		Timeout:             b.Timeout.Std(),
		MaxSessionEntries:   b.MaxSessionEntries,
		MaxSessionJSONBytes: b.MaxSessionJSONBytes.Bytes(),
	}
}

// startBrowse begins an initial browse load for a snapshot at the given caps. It
// switches to browseView immediately (showing a loading state with no tree yet)
// and kicks off the load; the built tree arrives later as a browseLoadedMsg.
func (m Model) startBrowse(repo, snapshotID string, limits model.BrowseLimits) (Model, tea.Cmd) {
	m.browseRepo = repo
	m.browseSnapshot = snapshotID
	m.browseResult = nil
	m.browseDir = "/"
	m.browseCursor = 0
	m.view = browseView
	return m.beginBrowseLoad(limits)
}

// loadMoreBrowse reloads the current snapshot from scratch with the next, doubled
// caps. It is a no-op when a load is already in flight (single-flight: repeated r
// must not stack crawls), when there is no tree yet, or when the stopping reason
// can't be raised any further. The old tree stays visible while the reload runs.
func (m Model) loadMoreBrowse() (Model, tea.Cmd) {
	if m.browsing || m.browseResult == nil {
		return m, nil
	}
	// The visible tree's own limits are the committed source of truth, so the
	// next caps are derived from them — never from a field bumped ahead of a load
	// that might be cancelled or fail. A cancelled/failed load-more therefore
	// leaves the caps exactly where the visible tree left them; the next r doubles
	// from there, not from a phantom intermediate level.
	if !model.CanLoadMore(m.browseResult.Reason, m.browseResult.Limits) {
		return m, nil
	}
	m.statusMsg = ""
	return m.beginBrowseLoad(model.NextBrowseLimits(m.browseResult.Limits))
}

// beginBrowseLoad sets up the generation/cancel/flags for a browse load and
// returns the command that runs it. It advances the generation token (so any
// superseded load's late result is discarded), cancels any prior in-flight
// browse, and derives a fresh cancel from m.ctx — a child of the program context
// so quitting still cascades, but back can cancel just the browse. The load runs
// off the refresh semaphore on purpose: a browse may run alongside refreshes.
func (m Model) beginBrowseLoad(limits model.BrowseLimits) (Model, tea.Cmd) {
	m.browseGen++
	if m.browseCancel != nil {
		m.browseCancel() // supersede any prior in-flight load
	}
	bctx, bcancel := context.WithCancel(m.ctx)
	m.browseCancel = bcancel
	m.browsing = true
	m.browseLoading = true

	gen := m.browseGen
	repo, snapshotID := m.browseRepo, m.browseSnapshot
	cmd := func() tea.Msg {
		res, err := m.app.BrowseSnapshot(bctx, repo, snapshotID, limits)
		if err != nil {
			return browseLoadedMsg{gen: gen, err: err}
		}
		return browseLoadedMsg{gen: gen, result: &res}
	}
	return m, cmd
}

// applyBrowseLoaded handles a finished browse load. A result whose generation no
// longer matches is from a cancelled or superseded crawl and is silently dropped,
// so an abandoned load can never resurrect browse state. On success the new tree
// replaces the old one and the current directory/selection are restored by path
// (falling back to the nearest existing ancestor when a path is gone). On an
// initial-load error there is no tree to show, so it returns to the detail view.
func (m Model) applyBrowseLoaded(msg browseLoadedMsg) Model {
	if msg.gen != m.browseGen {
		return m // stale: superseded or cancelled; discard
	}
	m.browsing = false
	m.browseLoading = false

	if msg.err != nil {
		// Browse errors carry only restic's redacted output (secrets masked) and
		// are never persisted. A load-more failure (below) keeps the old tree and
		// is cleared by browseBack on manual leave; an initial-load failure has no
		// tree to show, so we surface the message in the detail view we fall back
		// to. The new tree's caps are not committed either, so a failed load-more
		// keeps the visible tree's limits intact.
		m.statusMsg = "browse: " + firstLine(msg.err.Error())
		if m.browseResult == nil {
			// Initial load failed with no tree to fall back to: leave browse,
			// keeping the status so the user learns why it didn't open.
			m.view = detailView
			return m.clearBrowse()
		}
		return m // load-more failed: keep the old tree visible
	}

	// Capture the prior selection (load-more) before swapping trees.
	prevDir := m.browseDir
	prevSelected := m.selectedBrowsePath()

	m.browseResult = msg.result
	m.view = browseView

	if prevDir == "" || msg.result.Tree.ByPath[prevDir] == nil {
		m.browseDir = nearestBrowseDir(msg.result.Tree, prevDir)
	} else {
		m.browseDir = prevDir
	}
	m.browseCursor = m.indexOfBrowsePath(prevSelected)
	return m
}

// browseBack is the load-aware back for browseView, shared by q and esc. While a
// load-more is in flight over an existing tree, it cancels just that load and
// stays in browse with the old tree (advancing the generation so the cancelled
// load's late result is dropped). Otherwise — no load in flight, or an initial
// load that has no tree yet — it cancels any load, returns to the detail view,
// and clears all session browse state so no filenames linger.
func (m Model) browseBack() Model {
	// Drop any transient browse status (e.g. a load-more error) so a restic
	// message never lingers into the view we return to.
	m.statusMsg = ""
	if m.browseLoading && m.browseResult != nil {
		m.browseGen++
		if m.browseCancel != nil {
			m.browseCancel()
			m.browseCancel = nil
		}
		m.browsing = false
		m.browseLoading = false
		return m
	}
	m.browseGen++
	if m.browseCancel != nil {
		m.browseCancel()
	}
	m.view = detailView
	return m.clearBrowse()
}

// clearBrowse drops all session browse state. It is called on every exit from
// browse so no filename or path data lingers in the model after leaving.
func (m Model) clearBrowse() Model {
	m.browseResult = nil
	m.browseRepo = ""
	m.browseSnapshot = ""
	m.browseDir = ""
	m.browseCursor = 0
	m.browsing = false
	m.browseLoading = false
	m.browseCancel = nil
	return m
}

// handleBrowseKey routes keys while browsing. Navigation is paused during a load
// (the cursor would point into a tree that's about to be replaced); r reloads
// with raised caps; s shells into the browsed snapshot; back is load-aware.
func (m Model) handleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		return m.browseBack(), nil
	case key.Matches(msg, m.keys.Refresh): // r = load more
		return m.loadMoreBrowse()
	case key.Matches(msg, m.keys.Shell):
		if cmd := m.openShellCmd(m.browseSnapshotPtr()); cmd != nil {
			m.statusMsg = ""
			return m, cmd
		}
		return m, nil
	}

	if m.browseLoading {
		return m, nil // navigation is paused while the tree is being (re)built
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
	case key.Matches(msg, m.keys.Enter):
		m = m.openBrowseDir()
	case key.Matches(msg, m.keys.Parent):
		m = m.browseToParent()
	}
	return m, nil
}

// openBrowseDir descends into the selected entry when it is a directory; files
// have no action here (the shell is the way to read file contents).
func (m Model) openBrowseDir() Model {
	e := m.selectedBrowseEntry()
	if e == nil || !e.IsDir {
		return m
	}
	m.browseDir = e.Path
	m.browseCursor = 0
	return m
}

// browseToParent steps up one directory, restoring the cursor onto the child we
// came from so repeated enter/backspace feels like walking a path.
func (m Model) browseToParent() Model {
	if m.browseDir == "" || m.browseDir == "/" {
		return m
	}
	from := m.browseDir
	m.browseDir = path.Dir(m.browseDir)
	m.browseCursor = m.indexOfBrowsePath(from)
	return m
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

// currentBrowseDir returns the entry for the directory currently listed, falling
// back to the tree root when the stored path is missing (e.g. after a reload that
// dropped it). It is nil only when there is no tree at all.
func (m Model) currentBrowseDir() *model.BrowseEntry {
	if m.browseResult == nil || m.browseResult.Tree == nil {
		return nil
	}
	if e, ok := m.browseResult.Tree.ByPath[m.browseDir]; ok {
		return e
	}
	return m.browseResult.Tree.Root
}

// browseRowCount is the number of selectable entries in the current directory
// (the synthetic "more entries not loaded" row is shown but not selectable).
func (m Model) browseRowCount() int {
	if dir := m.currentBrowseDir(); dir != nil {
		return len(dir.Children)
	}
	return 0
}

func (m Model) selectedBrowseEntry() *model.BrowseEntry {
	dir := m.currentBrowseDir()
	if dir == nil {
		return nil
	}
	cur := m.browseCursor
	if cur < 0 || cur >= len(dir.Children) {
		return nil
	}
	return dir.Children[cur]
}

func (m Model) selectedBrowsePath() string {
	if e := m.selectedBrowseEntry(); e != nil {
		return e.Path
	}
	return ""
}

// indexOfBrowsePath returns the position of path p among the current directory's
// children, or 0 when it is absent. The current directory must already be set.
func (m Model) indexOfBrowsePath(p string) int {
	dir := m.currentBrowseDir()
	if dir == nil || p == "" {
		return 0
	}
	for i, c := range dir.Children {
		if c.Path == p {
			return i
		}
	}
	return 0
}

// nearestBrowseDir returns p if it exists in the tree, else its nearest existing
// ancestor, falling back to the root. It restores the current directory after a
// reload when the exact path is no longer present.
func nearestBrowseDir(tree *model.BrowseTree, p string) string {
	if tree == nil || p == "" {
		return "/"
	}
	for p != "/" && p != "." && p != "" {
		if _, ok := tree.ByPath[p]; ok {
			return p
		}
		p = path.Dir(p)
	}
	return "/"
}

// browseCanLoadMore reports whether the browse footer should advertise
// `r load more`: only in browse view, with a loaded tree, when raising the caps
// that stopped the current run could fetch more.
func (m Model) browseCanLoadMore() bool {
	if m.view != browseView || m.browseResult == nil {
		return false
	}
	return model.CanLoadMore(m.browseResult.Reason, m.browseResult.Limits)
}

// --- rendering ---

func (m Model) browseHeaderView() string {
	w, _ := m.effSize()
	label := "browse: " + m.browseRepo
	if id := shortID(m.browseSnapshot); id != "" {
		label += " · " + id
	}
	left := m.styles.title.Render(label)
	if m.browseResult != nil {
		left += "  " + m.browseStatusBadge()
	}
	right := m.styles.dim.Render("q back")
	return clip(m.spread(left, right), w)
}

// browseStatusBadge renders the scan's completeness in the heading: dim when
// complete, amber when the tree is only partial, so a truncated tree is never
// mistaken for the whole namespace.
func (m Model) browseStatusBadge() string {
	r := m.browseResult.Reason
	if r.Partial() {
		return m.styles.glyph[model.StatusAmber].Render(r.String())
	}
	return m.styles.dim.Render(r.String())
}

func (m Model) browseBody() string {
	w, _ := m.effSize()
	if m.browseResult == nil {
		return clip(m.styles.meta.Render("  loading…"), w)
	}
	dir := m.currentBrowseDir()

	pathLine := clip(m.styles.label.Render("Path")+m.styles.name.Render(browseDirLabel(m.browseDir)), w)
	summary := clip(m.styles.meta.Render("  "+m.browseSummaryLine()), w)
	return strings.Join([]string{pathLine, summary, m.browseList(dir, w)}, "\n")
}

// browseSummaryLine reports how much of the namespace the session holds: the
// loaded entry count, an approximate retained-memory figure (guidance only), and
// — while a load-more runs — that more is being fetched.
func (m Model) browseSummaryLine() string {
	res := m.browseResult
	parts := []string{
		fmt.Sprintf("%d entries", res.LoadedEntries),
		"~" + humanize.Bytes(res.ApproxRetainedBytes) + " retained",
	}
	if m.browseLoading {
		parts = append(parts, "loading more…")
	}
	return strings.Join(parts, " · ")
}

// browseList renders the scrolling window of the current directory's children,
// marking the cursor with the accent gutter, appending the synthetic
// "more entries not loaded" row for an incomplete directory, and a window note
// when the list is scrolled. Files show their size; directories a trailing slash.
func (m Model) browseList(dir *model.BrowseEntry, w int) string {
	if dir == nil {
		return clip(m.styles.meta.Render("  (no tree)"), w)
	}
	total := len(dir.Children)
	if total == 0 {
		if dir.HasMoreRow() {
			return m.browseMoreRow(w)
		}
		return clip(m.styles.meta.Render("  (empty)"), w)
	}

	cur := clampCursor(m.browseCursor, total)
	start, end := snapshotWindow(cur, total, m.browseVisible())

	lines := make([]string, 0, end-start+2)
	for i := start; i < end; i++ {
		lines = append(lines, m.browseRow(dir.Children[i], i == cur, w))
	}
	if dir.HasMoreRow() && end >= total {
		lines = append(lines, m.browseMoreRow(w))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), w))
	}
	return strings.Join(lines, "\n")
}

// browseRow renders one entry: an accent gutter on the cursor row, a dir/file
// icon, the (truncated) name, and a right-aligned size column for files. The
// whole line is clipped to width so a long name can't wrap.
func (m Model) browseRow(e *model.BrowseEntry, selected bool, w int) string {
	const sizeCol = 10
	indicator := "  "
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
	}
	icon := "  "
	name := e.Name
	size := ""
	if e.IsDir {
		icon = "▸ "
		name += "/"
	} else {
		size = humanize.Bytes(e.Size)
	}

	avail := w - 2 /*indicator*/ - 2 /*icon*/ - 2 /*gap*/ - sizeCol
	if avail < 1 {
		avail = 1
	}
	nameCell := truncate(name, avail)
	if selected {
		nameCell = m.styles.selected.Render(nameCell)
	}
	row := indicator + m.styles.dim.Render(icon) + padRight(nameCell, avail) +
		"  " + m.styles.meta.Render(fmt.Sprintf("%*s", sizeCol, size))
	return clip(row, w)
}

// browseMoreRow renders the synthetic row that marks an incomplete directory, so
// a truncated listing is never shown as if it were complete.
func (m Model) browseMoreRow(w int) string {
	return clip("  "+m.styles.dim.Render("… more entries not loaded"), w)
}

// browseVisible is how many entry rows the browse list shows at once: the height
// minus the header, gaps, footer, the two browse meta rows, and a reserved line
// for the scroll/more note. Floored at 1.
func (m Model) browseVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + browseMetaRows + 1
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

// browseDirLabel renders the current directory path for the header line, never
// empty so the label always has a value.
func browseDirLabel(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// shortID renders restic's 8-char short id from a full snapshot id, leaving an
// already-short id untouched.
func shortID(id string) string {
	if len(id) > snapIDWidth {
		return id[:snapIDWidth]
	}
	return id
}
