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

// detailMetaRows counts fixed repository metadata lines above the snapshot table.
const detailMetaRows = 7

// snapIDWidth is restic's eight-character short-ID column width.
const snapIDWidth = 8

// repoConfig returns the repository config for name.
func (m Model) repoConfig(name string) (config.Repo, bool) {
	for _, r := range m.app.Cfg.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}

// detailRow resolves the pinned repository by name so list reordering cannot
// change the open detail view.
func (m Model) detailRow() (app.RepoStatus, bool) {
	for _, r := range m.rows {
		if r.Name == m.detailName {
			return r, true
		}
	}
	return app.RepoStatus{}, false
}

// detailSnapshots returns a newest-first copy without reordering cached state.
func (m Model) detailSnapshots() []model.Snapshot {
	row, ok := m.detailRow()
	if !ok {
		return nil
	}
	return model.SortedSnapshotsNewestFirst(row.State.Snapshots)
}

// snapCount returns selectable display nodes, which collapse may reduce but
// grouping does not. Repository metadata uses the raw snapshot count instead.
func (m Model) snapCount() int {
	return len(m.snapDisplay().nodes)
}

// selectedSnapshot returns the selected display node's head snapshot, or nil.
func (m Model) selectedSnapshot() *model.Snapshot {
	n, ok := m.selectedNode()
	if !ok {
		return nil
	}
	head := n.head
	return &head
}

// openExtractSnapshot opens whole-snapshot extraction for the selected display
// head. Missing selections and setup errors remain in detail with a status.
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
	// Pre-0.17 snapshots lack summary size and render without a suffix.
	var srcSize int64
	if snap.Summary != nil {
		srcSize = snap.Summary.TotalBytesProcessed
	}
	sub, err := newExtractModel(m.app, m.ctx, m.seedTargetMemo(req), srcSize)
	if err != nil {
		// Planning errors identify fields without including paths.
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m, nil
	}
	m.statusMsg = ""
	m.extract = sub
	m.extractReturn = detailView
	m.view = extractView
	// Contains is available only for snapshots indexed during this session.
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

// detailMeta clips fixed repository facts so metadata stays at detailMetaRows.
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

// snapshotDetail renders selected-snapshot facts not already visible as table
// columns. Its shared line builder keeps layout reservation equal to output.
func (m Model) snapshotDetail(width int) string {
	s := m.selectedSnapshot()
	if s == nil {
		return ""
	}
	return strings.Join(m.snapshotDetailLines(width, *s), "\n")
}

// snapshotDetailLines builds the exact rows counted by detailSnapDetailRows.
func (m Model) snapshotDetailLines(width int, s model.Snapshot) []string {
	l := snapshotLayout(width, m.snapCollapseTree)

	ver := s.ProgramVersion
	if ver == "" {
		ver = "unknown version"
	}

	window, duration, hasWindow := snapshotBackupWindow(s)

	// Duration appears in Took when promoted and in Backup otherwise.
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

// snapshotBackupWindow returns formatted endpoints and duration when the
// snapshot summary contains a valid backup interval.
func snapshotBackupWindow(s model.Snapshot) (window, duration string, ok bool) {
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return "", "", false
	}
	const layout = "2006-01-02 15:04:05"
	return s.Summary.BackupStart.Format(layout) + " → " + s.Summary.BackupEnd.Format(layout),
		humanize.Duration(d), true
}

// snapshotChurn reports added, packed, and file counts. includeAdded controls
// whether bytes already owned by the Added column are repeated.
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

// field clips a fixed-label metadata row so it cannot wrap.
func (m Model) field(label, value string, width int) string {
	avail := max(width-2-labelWidth, 1)
	return clip("  "+m.styles.label.Render(label)+truncateWidth(value, avail), width)
}

// snapshotsHeadingText adds mark progress and non-default grouping or collapse
// state to the snapshot heading.
func (m Model) snapshotsHeadingText() string {
	head := "Snapshots"
	if n := len(m.detailMarks); n > 0 {
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

// snapshotTable renders a responsive, windowed snapshot table and dispatches
// empty, flat, or grouped display paths.
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

// snapshotRowLine renders a clipped node with a three-cell cursor/mark gutter.
// Folded peer marks remain visible on their head row.
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

// snapshotTableFlat renders a window of display nodes, including collapsed heads.
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

// snapshotTableGrouped windows section tokens while keeping the selected row's
// heading anchored and reserving header and scroll-note space.
func (m Model) snapshotTableGrouped(d snapDisplay, l snapLayout, width int) string {
	cur := clampCursor(m.snapCursor, len(d.nodes))
	tokens, headingPos := buildSnapTokens(d)
	showNote := m.detailWindowNoteVisible(m.detailSnapDetailVisible())

	// Section headings and separators consume the data-row budget.
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

	// With one line available, prefer the selected row over its heading.
	if max == 1 {
		out := []string{m.renderSnapToken(tokens[cursorPos], d, cur, l, width)}
		if note := m.snapTableScrollNote(cur, cur+1, len(d.nodes), width, showNote); note != "" {
			out = append(out, note)
		}
		return strings.Join(out, "\n")
	}

	// Resolve the selected section in flat-cursor order.
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

// renderSnapToken paints separators, section headings, and data rows. Fallback
// headings use a distinct style from matching real label values.
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

// snapTableScrollNote reports a partial node window.
func (m Model) snapTableScrollNote(start, end, total, width int, visible bool) string {
	if !visible {
		return ""
	}
	if start <= 0 && end >= total {
		return ""
	}
	return clip(m.styles.meta.Render(fmt.Sprintf("   showing %d–%d of %d", start+1, end, total)), width)
}

// snapLayout holds responsive snapshot columns, including collapse suffix space
// and optional Added and Took promotion.
type snapLayout struct {
	idWidth             int
	host, tags          int
	showAdded, showTook bool
}

const (
	snapAddedWidth          = 9
	snapTookWidth           = 6
	snapPromoFlexMin        = 37
	snapCollapseSuffixWidth = 3
)

// snapHeader uses data-row geometry plus the three-cell gutter.
func snapHeader(l snapLayout) string {
	return "   " + strings.Join(
		snapCells(l, snapRow{id: "ID", tm: "Time", host: "Hostname", size: "Size", added: "Added", took: "Took", tags: "Tags"}), "  ")
}

type snapRow struct {
	id, tm, host, size, added, took, tags string
}

// snapCells aligns headers and data, reserving collapse suffix space in the ID
// column and conditionally including Added and Took.
func snapCells(l snapLayout, r snapRow) []string {
	cells := []string{
		fmt.Sprintf("%-*s", l.idWidth, r.id),
		fmt.Sprintf("%-16s", r.tm),
		// Hostnames require display-width padding for wide runes.
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

// snapAdded returns deduplicated bytes added, or an em dash when unavailable.
func snapAdded(s model.Snapshot) string {
	if s.Summary == nil || s.Summary.DataAdded == nil {
		return emDash
	}
	return "+" + humanize.Bytes(*s.Summary.DataAdded)
}

// snapTook returns a width-limited backup duration, or an em dash when unavailable.
func snapTook(s model.Snapshot) string {
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return emDash
	}
	return truncateWidth(humanize.Duration(d), snapTookWidth)
}

// snapshotLayout reserves core columns, then promotes Added and Took while host
// and tags retain snapPromoFlexMin cells. Remaining width splits between them.
func snapshotLayout(width int, collapseOn bool) snapLayout {
	const indicator, timeW, sizeW, gaps = 3, 16, 9, 8
	var l snapLayout
	l.idWidth = snapIDWidth
	if collapseOn {
		// Reserve a uniform "+N" suffix slot, which may demote optional columns.
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

// detailSnapVisible returns snapshot capacity after visible fixed rows, floored
// at one.
func (m Model) detailSnapVisible() int {
	_, h := m.effSize()
	withSnapDetail := m.detailSnapDetailVisible()
	overhead := m.detailOverhead(withSnapDetail, m.detailWindowNoteVisible(withSnapDetail))
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

// detailSnapDetailVisible reports whether the selected-snapshot panel and at
// least one snapshot row fit.
func (m Model) detailSnapDetailVisible() bool {
	if m.snapCount() == 0 {
		return false
	}
	_, h := m.effSize()
	return h >= m.detailOverhead(true, false)+1
}

// detailWindowNoteVisible reports whether a scroll note and at least one
// snapshot row fit with the requested panel state.
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

// detailOverhead returns the fixed row count for the requested optional elements.
func (m Model) detailOverhead(withSnapDetail, withWindowNote bool) int {
	overhead := headerRows + 2*gapRows + m.footerRows() +
		detailMetaRows +
		1 +
		1 +
		1
	if withWindowNote {
		overhead++
	}
	if withSnapDetail {
		overhead += 1 +
			m.detailSnapDetailRows()
	}
	return overhead
}

func joinOrDash(vals []string) string {
	if len(vals) == 0 {
		return emDash
	}
	return strings.Join(vals, ", ")
}
