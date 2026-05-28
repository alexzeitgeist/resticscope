package tui

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"resticscope/internal/humanize"
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
// DB, so returning to an already-indexed snapshot in the same run is instant.

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
			// Non-blocking coalesced send: the restic stdout consumer must never
			// stall behind a full UI channel. If the buffer is full, drop this tick;
			// a later tick (or the final count) carries a fresher number.
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
		m.browseRows = rows
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
	m.browseRows = msg.rows
	m.browseCursor = m.indexOfBrowsePath(msg.selectPath)
	// Memoize the listing so a later return to this directory is served
	// synchronously (see beginListDir). The snapshot is immutable, so the entry
	// never needs invalidation; clearBrowse drops the whole map on leaving browse.
	if m.browseCache == nil {
		m.browseCache = make(map[string][]model.BrowseEntry)
	}
	m.browseCache[msg.dir] = msg.rows
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
	m.browseIndexed = false
	m.browseIndexN = 0
	m.browseIndexRate = 0
	m.browseRateBaseN = 0
	m.browseRateBaseAt = time.Time{}
	m.browseLoading = false
	m.browseCancel = nil
	m.browseProgress = nil
	return m
}

// handleBrowseKey routes keys while browsing. Back leaves (cancelling any
// in-flight work); s shells into the browsed snapshot. Navigation is paused while
// an index or listing is in flight, since the cursor would point into rows that
// are about to be replaced.
func (m Model) handleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
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
	}
	return m, nil
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

// --- rendering ---

func (m Model) browseHeaderView() string {
	w, _ := m.effSize()
	label := "browse: " + m.browseRepo
	if id := shortID(m.browseSnapshot); id != "" {
		label += " · " + id
	}
	left := m.styles.title.Render(label)
	right := m.styles.dim.Render("q back")
	return clip(m.spread(left, right), w)
}

func (m Model) browseBody() string {
	w, _ := m.effSize()
	pathLine := clip(m.styles.label.Render("Path")+m.styles.name.Render(browseDirLabel(m.browseDir)), w)
	summary := clip(m.styles.meta.Render("  "+m.browseSummaryLine()), w)
	// While the one-time index is still running there is no directory listing yet;
	// show only the path and the indexing summary (which keeps the cancel
	// affordance visible). Once indexed, the directory list joins them.
	if m.browseLoading && !m.browseIndexed {
		return strings.Join([]string{pathLine, summary}, "\n")
	}
	return strings.Join([]string{pathLine, summary, m.browseList(w)}, "\n")
}

// browseSummaryLine reports progress. While the one-time index runs it shows the
// running entry count and keeps the cancel affordance visible — a huge snapshot
// can take minutes, and the user must always see that esc/back aborts it.
// Otherwise it reports the current directory's entry count.
func (m Model) browseSummaryLine() string {
	if m.browseLoading && !m.browseIndexed {
		parts := []string{fmt.Sprintf("indexing… %d entries", m.browseIndexN)}
		if rate := browseIndexRateLabel(m.browseIndexRate); rate != "" {
			parts = append(parts, rate)
		}
		parts = append(parts, "esc/back cancels")
		return strings.Join(parts, " · ")
	}
	return fmt.Sprintf("%d entries", len(m.browseRows))
}

func browseIndexRateLabel(rate float64) string {
	if rate <= 0 {
		return ""
	}
	switch {
	case rate >= 1_000_000:
		return fmt.Sprintf("%.1fM/s", rate/1_000_000)
	case rate >= 1_000:
		return fmt.Sprintf("%.0fk/s", rate/1_000)
	case rate >= 10:
		return fmt.Sprintf("%.0f/s", rate)
	default:
		return fmt.Sprintf("%.1f/s", rate)
	}
}

// browseList renders the scrolling window of the current directory's rows as a
// responsive table: a dim column header, then rows marking the cursor with the
// accent gutter, and a window note when the list is scrolled. The header is shown
// for empty directories too so the table shape stays stable. The table renders at
// a capped working width (browseTableWidth) so a very wide terminal does not
// stretch the Name flex into a desert; the surrounding header/path/summary lines
// keep using the full terminal width.
func (m Model) browseList(w int) string {
	tw := browseTableWidth(w)
	l := browseLayout(tw)
	header := clip(m.styles.dim.Render(browseHeaderRow(l)), tw)

	total := len(m.browseRows)
	if total == 0 {
		return header + "\n" + clip(m.styles.meta.Render("  (empty)"), tw)
	}

	cur := clampCursor(m.browseCursor, total)
	start, end := snapshotWindow(cur, total, m.browseVisible())

	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		lines = append(lines, m.browseRow(&m.browseRows[i], i == cur, l, tw))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), tw))
	}
	return strings.Join(lines, "\n")
}

// browseTableMaxWidth bounds the file table's working width. Browse puts its only
// flexible column (Name) first, so left unbounded a wide terminal stretches Name
// and strands the metadata columns far to the right. (The snapshot table avoids
// this for free: its flex column, Tags, is last, so slack falls harmlessly at the
// trailing edge.) Capping the whole table — rather than just the Name column —
// states the decision once and keeps it correct if a future column is added.
const browseTableMaxWidth = 132

// browseTableWidth is the width the file table renders at: the terminal width,
// capped at browseTableMaxWidth so a wide terminal leaves trailing empty space
// rather than over-stretching the Name flex.
func browseTableWidth(w int) int {
	if w > browseTableMaxWidth {
		return browseTableMaxWidth
	}
	return w
}

// browseColLayout describes the browse table's variable geometry for a given
// width: the Name flex width and which of the optional Modified/Perms/Owner
// columns are promoted. It mirrors snapLayout so the two tables behave alike.
type browseColLayout struct {
	name      int
	showMod   bool
	showPerms bool
	showOwner bool
}

const (
	browseSizeWidth  = 10 // right-aligned Size column (matches the old single size column)
	browseModWidth   = 16 // "2006-01-02 15:04"
	browsePermsWidth = 10 // os.FileMode.String() is usually 10 chars; longer (sticky/setuid) is clipped
	browseOwnerWidth = 11 // "uid:gid"; real uids are small, a pathological pair is clipped
	browseNameMin    = 16 // the Name flex must stay at least this wide for a column to be promoted
)

// browseLayout sizes the browse table's columns to the total width. A two-cell
// indicator, the Name flex, and the fixed Size column (plus their two-space gaps)
// are always reserved. Modified then Perms then Owner are promoted in priority
// order, each only while the Name flex would stay at least browseNameMin wide
// afterwards; promotion stops at the first that won't fit so a lower-priority
// column never appears without a higher one. It mirrors snapshotLayout.
func browseLayout(width int) browseColLayout {
	const indicator, gap = 2, 2 // the gutter, and the one gap before Size
	baseFixed := indicator + browseSizeWidth + gap

	var l browseColLayout
	reservedExtra := 0
	for _, c := range []struct {
		width int
		on    *bool
	}{
		{browseModWidth, &l.showMod},
		{browsePermsWidth, &l.showPerms},
		{browseOwnerWidth, &l.showOwner},
	} {
		cost := c.width + 2 // the column plus one more two-space separator
		if width-baseFixed-reservedExtra-cost < browseNameMin {
			break
		}
		reservedExtra += cost
		*c.on = true
	}

	l.name = width - baseFixed - reservedExtra
	if l.name < 1 {
		l.name = 1
	}
	return l
}

// truncateWidth shortens s to at most max display cells, appending an ellipsis
// when it has to cut. Unlike truncate (which counts runes), it measures each
// rune's terminal width, so a filename with wide runes — CJK, emoji, or the
// fullwidth/small colon some apps substitute for ':' — still fits its column
// instead of shoving the metadata columns out of alignment. fmt's %-*s and
// rune-based truncate both miscount such names.
func truncateWidth(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	budget := max - 1 // reserve one cell for the ellipsis
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// browseCells formats one row's worth of columns — header or data — into the
// shared column order so both align: Name(l.name,left) · Size(10,right) ·
// [Modified(16,left)] · [Perms(10,left)] · [Owner(11,right)]. Callers join the
// result with two spaces. The Name cell carries arbitrary filenames, so it is
// truncated and padded by display width (filenames can hold wide runes); the
// fixed metadata cells are ASCII, so fmt's rune-count padding is exact for them.
func browseCells(l browseColLayout, name, size, mod, perms, owner string) []string {
	cells := []string{
		padRight(truncateWidth(name, l.name), l.name),
		fmt.Sprintf("%*s", browseSizeWidth, size),
	}
	if l.showMod {
		cells = append(cells, fmt.Sprintf("%-*s", browseModWidth, mod))
	}
	if l.showPerms {
		cells = append(cells, fmt.Sprintf("%-*s", browsePermsWidth, perms))
	}
	if l.showOwner {
		cells = append(cells, fmt.Sprintf("%*s", browseOwnerWidth, owner))
	}
	return cells
}

// browseHeaderRow is the dim column-label row, built from the same browseCells
// layout as the data rows (plus the two-cell gutter the rows get from their
// indicator) so labels line up with their values at every width.
func browseHeaderRow(l browseColLayout) string {
	return "  " + strings.Join(browseCells(l, "Name", "Size", "Modified", "Perms", "Owner"), "  ")
}

// browseRow renders one entry as a table row: an accent gutter on the cursor row,
// then the width-promoted Name/Size/Modified/Perms/Owner cells. Directories show a
// trailing slash and an em-dash size; missing metadata (no mtime, perms, or owner)
// renders as an em-dash too. The whole row content is clipped to width so a long
// name or value can't wrap, and the selected style covers the entire row.
func (m Model) browseRow(e *model.BrowseEntry, selected bool, l browseColLayout, tw int) string {
	icon := "  "
	name := e.Name
	if e.IsDir {
		icon = "▸ "
		name += "/"
	}
	// The name cell is icon + name; browseCells truncates and pads it to the flex
	// width by display width so wide-rune names don't misalign the columns.
	nameCell := icon + name

	size := "—"
	if !e.IsDir {
		size = humanize.Bytes(e.Size)
	}
	mod := "—"
	if !e.ModTime.IsZero() {
		mod = e.ModTime.Format("2006-01-02 15:04")
	}
	perms := "—"
	if e.Permissions != "" {
		perms = truncate(e.Permissions, browsePermsWidth)
	}
	owner := "—"
	if e.OwnerKnown {
		owner = truncate(fmt.Sprintf("%d:%d", e.UID, e.GID), browseOwnerWidth)
	}

	content := strings.Join(browseCells(l, nameCell, size, mod, perms, owner), "  ")
	indicator := "  "
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = m.styles.selected.Render(content)
	}
	return clip(indicator+content, tw)
}

// browseAuxRows is the worst-case number of fixed lines browseList renders around
// the scrolling window: the column header, plus — for a scrolled directory — the
// "showing N–M of T" note. browseVisible reserves both so the footer is never
// overlapped.
const browseAuxRows = 2

// browseVisible is how many entry rows the browse list shows at once: the height
// minus the app header, gaps, footer, the two browse meta rows, and the two
// auxiliary table lines (column header + scroll note). Floored at 1.
func (m Model) browseVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + browseMetaRows + browseAuxRows
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
