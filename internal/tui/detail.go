package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
)

// detailMetaRows is the number of fixed meta lines the detail body renders
// (Backend/Repository/Snapshots/Hosts/Program/Tags/Last); detailSnapVisible
// subtracts it from the height to size the scrolling snapshot window.
const detailMetaRows = 7

// snapIDWidth is the fixed width of the short-id column in the snapshot table;
// restic short ids are 8 hex chars. snapHeader, snapCells, and snapshotLayout all
// reserve exactly this width so the columns stay aligned.
const snapIDWidth = 8

// repoConfig returns the config.Repo backing the named row. The bool is false
// only if config and rows somehow disagree, which validation prevents.
func (m Model) repoConfig(name string) (config.Repo, bool) {
	for _, r := range m.app.Cfg.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}

// detailRow returns the repo the detail view is pinned to (by name, set on
// enter). Resolving by name rather than cursor keeps the detail view stable
// when an urgency sort or a grouping cycle reorders the list after a
// background refresh.
func (m Model) detailRow() (app.RepoStatus, bool) {
	for _, r := range m.rows {
		if r.Name == m.detailName {
			return r, true
		}
	}
	return app.RepoStatus{}, false
}

// detailSnapshots returns the detail repo's snapshots ordered newest-first (the
// order the detail view and snapCursor both use). It copies before sorting so
// the cached state's slice order is left untouched.
func (m Model) detailSnapshots() []model.Snapshot {
	row, ok := m.detailRow()
	if !ok {
		return nil
	}
	return model.SortedSnapshotsNewestFirst(row.State.Snapshots)
}

// snapCount is the number of selectable rows in the detail-view snapshot
// table: nodes in the current snapDisplay, NOT raw snapshots. Collapse folds
// peers into their head so the count shrinks; grouping never changes it. Both
// detail_keys cursor bounds and the windowing math reach for this. The meta
// "Snapshots: N" line wants the raw count and uses State.SnapshotCount
// directly — do not route it through here.
func (m Model) snapCount() int {
	return len(m.snapDisplay().nodes)
}

// selectedSnapshot returns the snapshot under the detail cursor, or nil when the
// repo has none. A shim over selectedNode that unwraps the head; callers that
// also need the peer set or the collapse count use selectedNode directly.
func (m Model) selectedSnapshot() *model.Snapshot {
	n, ok := m.selectedNode()
	if !ok {
		return nil
	}
	head := n.head
	return &head
}

// openExtractSnapshot launches the extract modal for the whole snapshot under
// the detail cursor (Source "/") — the detail-view counterpart of browse's
// openExtract. A nil selection or a request/setup error surfaces on the status
// line and stays in detail; only a clean construction switches to extractView.
// On a grouped or collapsed row the head snapshot is extracted, matching what
// enter/browse, `s`, and `i` act on.
func (m Model) openExtractSnapshot() (Model, tea.Cmd) {
	snap := m.selectedSnapshot()
	if snap == nil {
		m.statusMsg = "no snapshot selected"
		return m, nil
	}
	name, ok := m.actionRepo()
	if !ok {
		return m, nil
	}
	req, err := extractRequestFromSnapshot(name, snap)
	if err != nil {
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m, nil
	}
	// Display-only size for the review screen's Type line. Nil for pre-0.17
	// snapshots, where 0 renders without a size suffix.
	var srcSize int64
	if snap.Summary != nil {
		srcSize = snap.Summary.TotalBytesProcessed
	}
	sub, err := newExtractModel(m.app, m.ctx, m.seedTargetMemo(req), srcSize)
	if err != nil {
		// PlanExtractPaths returns a path-free ErrExtractInvalidRequest naming the
		// offending field, so the notice carries no path either.
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m, nil
	}
	m.statusMsg = ""
	m.extract = sub
	m.extractReturn = detailView
	m.view = extractView
	// The whole-snapshot Contains lookup answers only when this snapshot was
	// browsed (and so indexed) this session; otherwise the row stays absent.
	return m, m.extract.countsCmd()
}

func (m Model) detailTitle() string {
	row, _ := m.detailRow()
	return m.styles.title.Render("detail: "+row.Name) + " · " +
		m.styles.glyph[row.Status].Render(statusGlyph(row.Status)+" "+statusWord(row.Status))
}

func (m Model) detailBody() string {
	row, _ := m.detailRow()
	repo, _ := m.repoConfig(row.Name)
	w, _ := m.effSize()

	sections := []string{
		m.detailMeta(repo, row, w),
		clip(m.styles.heading.Render(m.snapshotsHeadingText()), w) + "\n" + m.snapshotTable(),
	}
	if m.detailSnapDetailVisible() {
		if sub := m.snapshotDetail(w); sub != "" {
			sections = append(sections, sub)
		}
	}
	return strings.Join(sections, "\n\n")
}

// detailMeta renders the fixed repo facts: where it lives and what was observed.
// Each value is clipped to the width left after the indent and label so a long
// repository string can't wrap and grow the block past detailMetaRows.
func (m Model) detailMeta(repo config.Repo, row app.RepoStatus, width int) string {
	st := row.State

	last := humanize.Ago(m.app.Clock.Now(), st.LastSnapshot)
	if !st.LastSnapshot.IsZero() {
		last = st.LastSnapshot.Format("2006-01-02 15:04") + " (" + last + ")"
	}

	repoURL := repo.RepositoryURL()
	lines := []string{
		m.field("Backend", repo.Backend(), width),
		m.field("Repository", repoURL, width),
		m.field("Snapshots", strconv.Itoa(st.SnapshotCount), width),
		m.field("Hosts", joinOrDash(st.Hosts), width),
		m.field("Program", joinOrDash(model.ObservedVersions(st.Snapshots)), width),
		m.field("Tags", joinOrDash(st.Tags), width),
		m.field("Last", last, width),
	}
	return strings.Join(lines, "\n")
}

// snapshotDetail renders a compact sub-panel describing the snapshot under
// the cursor — the per-backup data restic records that isn't already a table
// column: full id, restic version, username, backup window, packed size, and
// file counts, plus added bytes only while their column is off (snapshotLayout
// decides, so columns and panel never duplicate or drop that fact). detailBody
// calls it only when the repo has snapshots and the panel can fit. Every value
// is clipped to one line. snapshotDetailLines is the single source of the row
// set, so detailSnapDetailRows counts it and keeps the window above sized.
func (m Model) snapshotDetail(width int) string {
	s := m.selectedSnapshot()
	if s == nil {
		return ""
	}
	return strings.Join(m.snapshotDetailLines(width, *s), "\n")
}

// snapshotDetailLines builds the selected-snapshot panel's rendered lines for s.
// detailSnapDetailRows counts them, so the drawn height and the height the
// layout reserves for the panel can never disagree.
func (m Model) snapshotDetailLines(width int, s model.Snapshot) []string {
	l := snapshotLayout(width, m.snapCollapseTree)

	ver := s.ProgramVersion
	if ver == "" {
		ver = "unknown version"
	}

	window, duration, hasWindow := snapshotBackupWindow(s)

	// Duration is owned by the Took column when visible; otherwise the backup
	// window row carries it. The heading only ever notes a missing duration — a
	// valid one always lands in the column or the window row.
	heading := fmt.Sprintf("Selected · %s · %s", s.ShortID, ver)
	if !l.showTook && !hasWindow {
		heading += " · took —"
	}

	lines := []string{
		clip(m.styles.heading.Render(heading), width),
		m.field("ID", s.ID, width),
	}
	if s.Username != "" {
		lines = append(lines, m.field("User", s.Username, width))
	}
	if hasWindow {
		if !l.showTook {
			window += " (" + duration + ")"
		}
		lines = append(lines, m.field("Backup", window, width))
	}
	churn := "no summary"
	if s.Summary != nil {
		churn = snapshotChurn(s.Summary, !l.showAdded)
	}
	return append(lines, m.field("Churn", churn, width))
}

// snapshotBackupWindow formats the snapshot's start → end timestamps and its
// humanized duration. ok is false when the snapshot has no summary or no valid
// duration — the same condition SnapshotBackupDuration reports, so a true ok
// guarantees Summary is set.
func snapshotBackupWindow(s model.Snapshot) (window, duration string, ok bool) {
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return "", "", false
	}
	const layout = "2006-01-02 15:04:05"
	return s.Summary.BackupStart.Format(layout) + " → " + s.Summary.BackupEnd.Format(layout),
		humanize.Duration(d), true
}

// snapshotChurn renders the per-backup churn line for the bottom panel. When
// includeAdded is true the panel owns the added bytes (no Added column), so it
// leads with "+X added (Y packed)"; when false the Added column already shows
// the added bytes, so it omits that piece and leads with bare "Y packed".
func snapshotChurn(sum *model.SnapshotSummary, includeAdded bool) string {
	if sum == nil {
		return "no summary"
	}
	parts := make([]string, 0, 4)
	if includeAdded { //nolint:nestif // a 2×2 matrix (includeAdded × which DataAdded* field is set); the nesting mirrors that structure, and flattening it would duplicate the conditions
		if sum.DataAdded != nil {
			added := "+" + humanize.Bytes(*sum.DataAdded) + " added"
			if sum.DataAddedPacked != nil {
				added += " (" + humanize.Bytes(*sum.DataAddedPacked) + " packed)"
			}
			parts = append(parts, added)
		} else if sum.DataAddedPacked != nil {
			parts = append(parts, "+"+humanize.Bytes(*sum.DataAddedPacked)+" packed")
		}
	} else if sum.DataAddedPacked != nil {
		parts = append(parts, humanize.Bytes(*sum.DataAddedPacked)+" packed")
	}
	if sum.FilesNew != nil {
		parts = append(parts, fmt.Sprintf("%d new", *sum.FilesNew))
	}
	if sum.FilesChanged != nil {
		parts = append(parts, fmt.Sprintf("%d changed", *sum.FilesChanged))
	}
	if sum.TotalFilesProcessed != nil {
		parts = append(parts, humanize.Count(*sum.TotalFilesProcessed, "file", "files"))
	}
	if len(parts) == 0 {
		if includeAdded {
			return "churn unavailable"
		}
		return emDash
	}
	return strings.Join(parts, " · ")
}

// field renders one meta line: a two-space indent, the fixed-width label, then
// the value clipped to whatever width is left. The whole line is clipped too, so
// even a pane too narrow for the label itself can't wrap.
func (m Model) field(label, value string, width int) string {
	avail := max(width-2-labelWidth, 1)
	return clip("  "+m.styles.label.Render(label)+truncateWidth(value, avail), width)
}

// snapshotsHeadingText is the heading row above the snapshot table. The bare
// label "Snapshots" gains a marks-status suffix while the diff-mark FIFO is
// non-empty so the user sees their progress toward a valid pair without losing
// a body row to a dedicated summary line. Transient group/collapse state is
// surfaced as a "· group: host" / "· collapse on" suffix only when it differs
// from the per-visit defaults (both off), keeping the heading clean for users
// who never touch g or c.
func (m Model) snapshotsHeadingText() string {
	head := "Snapshots"
	if n := len(m.detailMarks); n > 0 {
		// State only — the t/d key hints live in the footer key bar, not here.
		head = fmt.Sprintf("Snapshots · %d/2 marked", n)
	}
	if lbl := m.snapGroupMode.label(); lbl != "" {
		head += " · group: " + lbl
	}
	if m.snapCollapseTree {
		head += " · collapse on"
	}
	return head
}

// snapshotTable renders a column header and a scrolling window of snapshots,
// newest first, marking the selected row with the accent gutter. The columns size
// to the terminal width (snapshotLayout) and the window to its height
// (detailSnapVisible) so the table fills the pane without wrapping. Enter on
// the selection opens a shell scoped to it. The body is a thin dispatcher:
// empty repos render the "no snapshots" placeholder; grouped displays go
// through the section-aware path; everything else hits the flat path.
func (m Model) snapshotTable() string {
	w, _ := m.effSize()
	l := snapshotLayout(w, m.snapCollapseTree)
	header := clip(m.styles.dim.Render(snapHeader(l)), w)

	d := m.snapDisplay()
	if len(d.nodes) == 0 {
		return header + "\n" + clip(m.styles.meta.Render("   no snapshots"), w)
	}
	if d.sections == nil {
		return header + "\n" + m.snapshotTableFlat(d, l, w)
	}
	return header + "\n" + m.snapshotTableGrouped(d, l, w)
}

// snapshotRowLine formats one node into a clipped table line with the 3-cell
// cursor/mark gutter. The gutter carries the cursor accent (cell 1 = ▎ when
// selected), the mark glyph (cell 2 = * when this node is in the diff FIFO),
// and a spacer (cell 3) so the mark doesn't butt against the ID column. Both
// indicators can show at once (▎*); marks live on the head row even when the
// mark originally targeted a now-folded peer (isNodeMarked ORs head + peers).
func (m Model) snapshotRowLine(node snapNode, l snapLayout, width int, selected bool) string {
	s := node.head
	size := emDash
	if s.Summary != nil {
		size = humanize.Bytes(s.Summary.TotalBytesProcessed)
	}
	content := strings.Join(snapCells(l, snapRow{
		id:    idCell(s.ShortID, node.count),
		tm:    s.Time.Format("2006-01-02 15:04"),
		host:  truncateWidth(s.Hostname, l.host),
		size:  size,
		added: snapAdded(s),
		took:  snapTook(s),
		tags:  truncateWidth(strings.Join(s.Tags, ","), l.tags),
	}), "  ")
	left := " "
	right := " "
	if selected {
		left = m.styles.gutter.Render("▎")
		content = m.styles.selected.Render(content)
	}
	if m.isNodeMarked(node) {
		right = m.styles.chgAdded.Render("*")
	}
	return clip(left+right+" "+content, width)
}

// snapshotTableFlat renders the windowed flat path: the same simple scroll
// behavior as before grouping existed, but iterating snapDisplay.nodes so
// collapse can fold consecutive same-tree rows into "(+N)" heads.
func (m Model) snapshotTableFlat(d snapDisplay, l snapLayout, width int) string {
	cur := clampCursor(m.snapCursor, len(d.nodes))
	start, end := scrollWindow(cur, len(d.nodes), m.detailSnapVisible())

	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		lines = append(lines, m.snapshotRowLine(d.nodes[i], l, width, i == cur))
	}
	if (start > 0 || end < len(d.nodes)) && m.detailWindowNoteVisible(m.detailSnapDetailVisible()) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("   showing %d–%d of %d", start+1, end, len(d.nodes))), width))
	}
	return strings.Join(lines, "\n")
}

// snapshotTableGrouped renders the section-aware path: a flattened token
// stream (heading / blank / row) windowed by groupedWindow so the cursor's
// heading is always anchored. The line budget excludes the table header and
// the optional scroll note so the heading-anchored window can't push the
// selected row off the bottom of the pane.
func (m Model) snapshotTableGrouped(d snapDisplay, l snapLayout, width int) string {
	cur := clampCursor(m.snapCursor, len(d.nodes))
	tokens, headingPos := buildSnapTokens(d)
	showNote := m.detailWindowNoteVisible(m.detailSnapDetailVisible())

	// detailSnapVisible already excludes the scroll-note row from the data-row
	// budget (detailOverhead bakes the note in when it would be visible), so
	// max here is the count of body lines we can emit before the optional note.
	// Headings and blank separators count against this budget in grouped mode —
	// the user sees fewer data rows when sections take up space, mirroring
	// renderGroupedList's contract on the list view.
	max := m.detailSnapVisible()
	if max < 1 {
		max = 1
	}

	if max >= len(tokens) {
		out := make([]string, 0, len(tokens)+1)
		for _, t := range tokens {
			out = append(out, m.renderSnapToken(t, d, cur, l, width))
		}
		if note := m.snapTableScrollNote(0, len(d.nodes), len(d.nodes), width, showNote); note != "" {
			out = append(out, note)
		}
		return strings.Join(out, "\n")
	}

	cursorPos := 0
	for i, t := range tokens {
		if t.kind == snapTokRow && t.data == cur {
			cursorPos = i
			break
		}
	}

	// When only one content line fits, render the selected data row alone —
	// mirrors renderGroupedList. groupedWindow can otherwise return a
	// heading-only window and hide the selected row.
	if max == 1 {
		out := []string{m.renderSnapToken(tokens[cursorPos], d, cur, l, width)}
		if note := m.snapTableScrollNote(cur, cur+1, len(d.nodes), width, showNote); note != "" {
			out = append(out, note)
		}
		return strings.Join(out, "\n")
	}

	// Find the heading position for the cursor's section by walking sections
	// in flat-cursor order — same logic as renderGroupedList.
	cursorSec, rowsBefore := 0, 0
	for si, sec := range d.sections {
		if cur < rowsBefore+len(sec.nodes) {
			cursorSec = si
			break
		}
		rowsBefore += len(sec.nodes)
	}
	hPos := headingPos[cursorSec]

	start, end, prependHeading := groupedWindow(cursorPos, hPos, max, len(tokens))

	out := make([]string, 0, max+2)
	if prependHeading {
		out = append(out, m.renderSnapToken(tokens[hPos], d, cur, l, width))
	}
	for i := start; i < end; i++ {
		out = append(out, m.renderSnapToken(tokens[i], d, cur, l, width))
	}

	dataStart, dataEnd := cur, cur+1
	dataStartFound := false
	for i := start; i < end; i++ {
		if tokens[i].kind == snapTokRow {
			if !dataStartFound {
				dataStart = tokens[i].data
				dataStartFound = true
			}
			dataEnd = tokens[i].data + 1
		}
	}
	if note := m.snapTableScrollNote(dataStart, dataEnd, len(d.nodes), width, showNote); note != "" {
		out = append(out, note)
	}
	return strings.Join(out, "\n")
}

// renderSnapToken paints one token in the grouped stream: a blank separator,
// a section heading with its raw snapshot count, or a data row. The heading
// uses meta style for the noKey fallback section so a real label value that
// matches the fallback title can't visually merge with it.
func (m Model) renderSnapToken(t snapTok, d snapDisplay, cur int, l snapLayout, width int) string {
	switch t.kind {
	case snapTokBlank:
		return ""
	case snapTokHeading:
		sec := d.sections[t.section]
		titleStyle := m.styles.heading
		if sec.noKey {
			titleStyle = m.styles.meta
		}
		title := titleStyle.Render(sec.title) + " " + m.styles.dim.Render(fmt.Sprintf("(%d)", sec.rawCount))
		return clip(title, width)
	case snapTokRow:
		sec := d.sections[t.section]
		return m.snapshotRowLine(sec.nodes[t.node], l, width, t.data == cur)
	default:
		return ""
	}
}

// snapTableScrollNote is the snapshot-table variant of group.go's scrollNote:
// "showing N–M of T" over node counts, or "" when the window covers all nodes.
func (m Model) snapTableScrollNote(start, end, total, width int, visible bool) string {
	if !visible {
		return ""
	}
	if start <= 0 && end >= total {
		return ""
	}
	return clip(m.styles.meta.Render(fmt.Sprintf("   showing %d–%d of %d", start+1, end, total)), width)
}

// snapLayout describes the snapshot table's variable geometry for a given width:
// the host and tags column widths, the total ID column width (snapIDWidth in
// the default case; widened to make room for a "+N" suffix slot when collapse
// is on), and whether the optional Added/Took columns are promoted. snapshotTable
// and snapshotDetail both derive it from snapshotLayout so the columns and the
// bottom panel always agree on what's shown where.
type snapLayout struct {
	idWidth             int // snapIDWidth, or snapIDWidth+1+snapCollapseSuffixWidth when collapse is on
	host, tags          int
	showAdded, showTook bool
}

const (
	snapAddedWidth          = 9  // "+1023 GiB" target width, right-aligned like Size
	snapTookWidth           = 6  // "12h59m" target width; truncate longer durations to this
	snapPromoFlexMin        = 37 // host+tags cells that must remain after promoting a column; absorbs the gutter's spacer cell so Added/Took still promote at 92/100
	snapCollapseSuffixWidth = 3  // "+N" suffix slot reserved next to ID when collapse is on; fits +99 cleanly, wider counts widen that one row only
)

// snapHeader is the dim column-label row for the snapshot table, built from the
// same snapCells layout as the data rows (plus the three-cell gutter the rows
// get from their indicator) so labels line up with their values at every width.
func snapHeader(l snapLayout) string {
	return "   " + strings.Join(
		snapCells(l, snapRow{id: "ID", tm: "Time", host: "Hostname", size: "Size", added: "Added", took: "Took", tags: "Tags"}), "  ")
}

// snapRow holds one row's raw column values for snapCells (header or data).
type snapRow struct {
	id, tm, host, size, added, took, tags string
}

// snapCells formats one row's worth of columns — header or data — into the
// shared column order, so both are guaranteed to align: ID(l.idWidth,left) ·
// Time(16,left) · Hostname(host,left) · Size(9,right) · [Added(9,right)] ·
// [Took(6,right)] · Tags(flex,left). The ID column widens when collapse is on
// to reserve a "+N" suffix slot — every row pads to the same width so columns
// past ID stay aligned. Callers join the result with two spaces.
func snapCells(l snapLayout, r snapRow) []string {
	cells := []string{
		fmt.Sprintf("%-*s", l.idWidth, r.id),
		fmt.Sprintf("%-16s", r.tm),
		// Hostname can hold wide runes; pad by display width, not fmt's rune count.
		padRight(r.host, l.host),
		fmt.Sprintf("%9s", r.size),
	}
	if l.showAdded {
		cells = append(cells, fmt.Sprintf("%*s", snapAddedWidth, r.added))
	}
	if l.showTook {
		cells = append(cells, fmt.Sprintf("%*s", snapTookWidth, r.took))
	}
	return append(cells, r.tags)
}

// snapAdded is the right-aligned Added cell value: deduped bytes this run added,
// or an em-dash when the summary (pre-0.17) or the field is absent.
func snapAdded(s model.Snapshot) string {
	if s.Summary == nil || s.Summary.DataAdded == nil {
		return emDash
	}
	return "+" + humanize.Bytes(*s.Summary.DataAdded)
}

// snapTook is the right-aligned Took cell value: how long the backup ran,
// truncated to snapTookWidth so a long duration (e.g. 1000h00m) can't widen the
// column and shove Tags out of alignment. Em-dash when unavailable.
func snapTook(s model.Snapshot) string {
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return emDash
	}
	return truncateWidth(humanize.Duration(d), snapTookWidth)
}

// snapshotLayout sizes the snapshot table's variable columns to the total width. A
// three-cell indicator, the fixed-width short-id, 16-cell time, 9-cell size, and
// their two-space gaps are always reserved. Added then Took are promoted in
// priority order, each only while the host+tags flex area would stay usable
// (snapPromoFlexMin) afterwards; promotion stops at the first that won't fit so
// a lower-priority column never appears without a higher one. With the constants
// here Added lands at width 92 and Took at 100. Whatever flex remains splits
// into a host column clamped to 8–24 and tags taking the rest. Promotion is
// shared with browseLayout via promoteColumns; only the host/tags split below is
// snapshot-specific.
func snapshotLayout(width int, collapseOn bool) snapLayout {
	const indicator, timeW, sizeW, gaps = 3, 16, 9, 8
	var l snapLayout
	l.idWidth = snapIDWidth
	if collapseOn {
		// Widen ID by one space + the "+N" slot so the cell can carry
		// "shortid +N" on collapsed rows and "shortid   " (blank slot) on
		// uncollapsed rows — every row pads to l.idWidth so subsequent
		// columns align. The flex floor (snapPromoFlexMin) tightens
		// accordingly, which may demote Added/Took on narrow terminals.
		l.idWidth = snapIDWidth + 1 + snapCollapseSuffixWidth
	}
	baseFixed := indicator + l.idWidth + timeW + sizeW + gaps

	reservedExtra := promoteColumns(width, baseFixed, snapPromoFlexMin, []optionalCol{
		{snapAddedWidth, &l.showAdded},
		{snapTookWidth, &l.showTook},
	})

	rest := max(width-baseFixed-reservedExtra, 2)
	l.host = min(max(rest/2, 8), 24)
	l.tags = max(rest-l.host, 1)
	return l
}

// detailSnapVisible is how many snapshot data rows the detail view shows at once.
// It subtracts the view's fixed rows, including the selected-snapshot sub-panel
// and scroll note only when they can fit. Floored at 1, which sets the detail
// view's minimum usable height — below it the footer scrolls off.
func (m Model) detailSnapVisible() int {
	_, h := m.effSize()
	withSnapDetail := m.detailSnapDetailVisible()
	overhead := m.detailOverhead(withSnapDetail, m.detailWindowNoteVisible(withSnapDetail))
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

func (m Model) detailSnapDetailVisible() bool {
	if m.snapCount() == 0 {
		return false
	}
	_, h := m.effSize()
	return h >= m.detailOverhead(true, false)+1
}

func (m Model) detailWindowNoteVisible(withSnapDetail bool) bool {
	_, h := m.effSize()
	return h >= m.detailOverhead(withSnapDetail, true)+1
}

func (m Model) detailSnapDetailRows() int {
	s := m.selectedSnapshot()
	if s == nil {
		return 0
	}
	w, _ := m.effSize()
	return len(m.snapshotDetailLines(w, *s))
}

func (m Model) detailOverhead(withSnapDetail, withWindowNote bool) int {
	overhead := headerRows + 2*gapRows + m.footerRows() +
		detailMetaRows + // the seven meta lines
		1 + // the blank line between the meta block and the heading
		1 + // the "Snapshots" heading
		1 // the table's column-header row
	if withWindowNote {
		overhead++ // the "showing N–M of T" note
	}
	if withSnapDetail {
		overhead += 1 + // the blank line between the table and the snapshot sub-panel
			m.detailSnapDetailRows() // the selected-snapshot sub-panel
	}
	return overhead
}

func joinOrDash(vals []string) string {
	if len(vals) == 0 {
		return emDash
	}
	return strings.Join(vals, ", ")
}
