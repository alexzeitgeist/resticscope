package tui

import (
	"fmt"
	"strings"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// detailMetaRows is the number of fixed meta lines the detail body renders
// (Endpoint/Bucket/Snapshots/Hosts/Versions/Tags/Last); detailSnapVisible
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
// enter). Resolving by name rather than cursor keeps the detail view stable when
// a size/staleness sort reorders the list after a background refresh.
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
	row, _ := m.detailRow()
	return model.SortedSnapshotsNewestFirst(row.State.Snapshots)
}

func (m Model) snapCount() int {
	row, _ := m.detailRow()
	return len(row.State.Snapshots)
}

// selectedSnapshot returns the snapshot under the detail cursor, or nil when the
// repo has none. The cursor is clamped on read so a refresh that shrinks the
// list can never index out of range.
func (m Model) selectedSnapshot() *model.Snapshot {
	return m.selectedSnapshotFrom(m.detailSnapshots())
}

func (m Model) selectedSnapshotFrom(snaps []model.Snapshot) *model.Snapshot {
	if len(snaps) == 0 {
		return nil
	}
	cur := m.snapCursor
	if cur >= len(snaps) {
		cur = len(snaps) - 1
	}
	return &snaps[cur]
}

func (m Model) detailHeaderView() string {
	row, _ := m.detailRow()
	left := m.styles.title.Render(row.Name) + "  " +
		m.styles.glyph[row.Status].Render(statusGlyph(row.Status)+" "+string(row.Status))
	right := m.styles.dim.Render("q back")
	w, _ := m.effSize()
	return clip(m.spread(left, right), w)
}

func (m Model) detailBody() string {
	row, _ := m.detailRow()
	repo, _ := m.repoConfig(row.Name)
	w, _ := m.effSize()
	snaps := m.detailSnapshots()

	sections := []string{
		m.detailMeta(repo, row, w),
		clip(m.styles.heading.Render("Snapshots"), w) + "\n" + m.snapshotTable(snaps),
	}
	if m.detailSnapDetailVisible() {
		if sub := m.snapshotDetail(w, snaps); sub != "" {
			sections = append(sections, sub)
		}
	}
	return strings.Join(sections, "\n\n")
}

// detailMeta renders the fixed repo facts: where it lives and what was observed.
// Each value is clipped to the width left after the indent and label so a long
// endpoint or bucket can't wrap and grow the block past detailMetaRows.
func (m Model) detailMeta(repo config.Repo, row app.RepoStatus, width int) string {
	st := row.State

	last := humanize.Ago(m.app.Clock.Now(), st.LastSnapshot)
	if !st.LastSnapshot.IsZero() {
		last = st.LastSnapshot.Format("2006-01-02 15:04") + " (" + last + ")"
	}

	lines := []string{
		m.field("Endpoint", repo.Endpoint, width),
		m.field("Bucket", bucketLabel(repo), width),
		m.field("Snapshots", fmt.Sprintf("%d", st.SnapshotCount), width),
		m.field("Hosts", joinOrDash(st.Hosts), width),
		m.field("Versions", joinOrDash(model.ObservedVersions(st.Snapshots)), width),
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
// is clipped to one line; detailSnapDetailRows mirrors the conditional rows so
// the snapshot window above stays correctly sized.
func (m Model) snapshotDetail(width int, snaps []model.Snapshot) string {
	s := m.selectedSnapshotFrom(snaps)
	if s == nil {
		return ""
	}
	l := snapshotLayout(width)

	ver := s.ProgramVersion
	if ver == "" {
		ver = "unknown version"
	}

	backupWindow, hasBackupWindow := snapshotBackupWindow(*s)

	// Duration lives in the backup-window row when it is available, and otherwise
	// in the Took column when that column is present. Surface it in the heading
	// only when neither place already owns it.
	heading := fmt.Sprintf("Selected · %s · %s", s.ShortID, ver)
	if !l.showTook && !hasBackupWindow {
		heading += " · took " + snapshotDurationLabel(*s)
	}

	churn := "no summary"
	if sum := s.Summary; sum != nil {
		churn = snapshotChurn(sum, !l.showAdded)
	}

	lines := []string{
		clip(m.styles.heading.Render(heading), width),
		m.field("ID", s.ID, width),
	}
	if s.Username != "" {
		lines = append(lines, m.field("User", s.Username, width))
	}
	if hasBackupWindow {
		lines = append(lines, m.field("Backup", backupWindow, width))
	}
	lines = append(lines, m.field("Churn", churn, width))
	return strings.Join(lines, "\n")
}

func snapshotDurationLabel(s model.Snapshot) string {
	if d, ok := model.SnapshotBackupDuration(s); ok {
		return humanize.Duration(d)
	}
	return "—"
}

func snapshotBackupWindow(s model.Snapshot) (string, bool) {
	if s.Summary == nil {
		return "", false
	}
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return "", false
	}
	const layout = "2006-01-02 15:04:05"
	return s.Summary.BackupStart.Format(layout) + " → " +
		s.Summary.BackupEnd.Format(layout) + " (" + humanize.Duration(d) + ")", true
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
	if includeAdded {
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
		parts = append(parts, fmt.Sprintf("%d files", *sum.TotalFilesProcessed))
	}
	if len(parts) == 0 {
		if includeAdded {
			return "churn unavailable"
		}
		return "—"
	}
	return strings.Join(parts, " · ")
}

// field renders one meta line: a two-space indent, the fixed-width label, then
// the value clipped to whatever width is left. The whole line is clipped too, so
// even a pane too narrow for the label itself can't wrap.
func (m Model) field(label, value string, width int) string {
	avail := width - 2 - labelWidth
	if avail < 1 {
		avail = 1
	}
	return clip("  "+m.styles.label.Render(label)+truncate(value, avail), width)
}

// snapshotTable renders a column header and a scrolling window of snapshots,
// newest first, marking the selected row with the accent gutter. The columns size
// to the terminal width (snapshotLayout) and the window to its height (detailSnapVisible)
// so the table fills the pane without wrapping. Enter on the selection opens a
// shell scoped to it.
func (m Model) snapshotTable(snaps []model.Snapshot) string {
	w, _ := m.effSize()
	l := snapshotLayout(w)
	header := clip(m.styles.dim.Render(snapHeader(l)), w)

	if len(snaps) == 0 {
		return header + "\n" + clip(m.styles.meta.Render("  no snapshots"), w)
	}

	cur := m.snapCursor
	if cur >= len(snaps) {
		cur = len(snaps) - 1
	}
	start, end := snapshotWindow(cur, len(snaps), m.detailSnapVisible())

	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		s := snaps[i]
		size := "—"
		if s.Summary != nil {
			size = humanize.Bytes(s.Summary.TotalBytesProcessed)
		}
		content := strings.Join(snapCells(l,
			s.ShortID,
			s.Time.Format("2006-01-02 15:04"),
			truncate(s.Hostname, l.host),
			size,
			snapAdded(s),
			snapTook(s),
			truncate(strings.Join(s.Tags, ","), l.tags),
		), "  ")
		indicator := "  "
		if i == cur {
			indicator = m.styles.gutter.Render("▎") + " "
			content = m.styles.selected.Render(content)
		}
		lines = append(lines, clip(indicator+content, w))
	}
	if (start > 0 || end < len(snaps)) && m.detailWindowNoteVisible(m.detailSnapDetailVisible()) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(snaps))), w))
	}
	return strings.Join(lines, "\n")
}

// snapLayout describes the snapshot table's variable geometry for a given width:
// the host and tags column widths, and whether the optional Added/Took columns
// are promoted. snapshotTable and snapshotDetail both derive it from
// snapshotLayout so the columns and the bottom panel always agree on what's
// shown where.
type snapLayout struct {
	host, tags          int
	showAdded, showTook bool
}

const (
	snapAddedWidth   = 9  // "+1023 GiB" target width, right-aligned like Size
	snapTookWidth    = 6  // "12h59m" target width; truncate longer durations to this
	snapPromoFlexMin = 38 // host+tags cells that must remain after promoting a column
)

// snapHeader is the dim column-label row for the snapshot table, built from the
// same snapCells layout as the data rows (plus the two-cell gutter the rows get
// from their indicator) so labels line up with their values at every width.
func snapHeader(l snapLayout) string {
	return "  " + strings.Join(
		snapCells(l, "ID", "Time", "Hostname", "Size", "Added", "Took", "Tags"), "  ")
}

// snapCells formats one row's worth of columns — header or data — into the
// shared column order, so both are guaranteed to align: ID(8,left) · Time(16,
// left) · Hostname(host,left) · Size(9,right) · [Added(9,right)] ·
// [Took(6,right)] · Tags(flex,left). Callers join the result with two spaces.
func snapCells(l snapLayout, id, tm, host, size, added, took, tags string) []string {
	cells := []string{
		fmt.Sprintf("%-*s", snapIDWidth, id),
		fmt.Sprintf("%-16s", tm),
		fmt.Sprintf("%-*s", l.host, host),
		fmt.Sprintf("%9s", size),
	}
	if l.showAdded {
		cells = append(cells, fmt.Sprintf("%*s", snapAddedWidth, added))
	}
	if l.showTook {
		cells = append(cells, fmt.Sprintf("%*s", snapTookWidth, took))
	}
	return append(cells, tags)
}

// snapAdded is the right-aligned Added cell value: deduped bytes this run added,
// or an em-dash when the summary (pre-0.17) or the field is absent.
func snapAdded(s model.Snapshot) string {
	if s.Summary == nil || s.Summary.DataAdded == nil {
		return "—"
	}
	return "+" + humanize.Bytes(*s.Summary.DataAdded)
}

// snapTook is the right-aligned Took cell value: how long the backup ran,
// truncated to snapTookWidth so a long duration (e.g. 1000h00m) can't widen the
// column and shove Tags out of alignment. Em-dash when unavailable.
func snapTook(s model.Snapshot) string {
	d, ok := model.SnapshotBackupDuration(s)
	if !ok {
		return "—"
	}
	return truncate(humanize.Duration(d), snapTookWidth)
}

// snapshotLayout sizes the snapshot table's variable columns to the total width. A
// two-cell indicator, the fixed-width short-id, 16-cell time, 9-cell size, and
// their two-space gaps are always reserved. Added then Took are promoted in
// priority order, each only while the host+tags flex area would stay usable
// (snapPromoFlexMin) afterwards; promotion stops at the first that won't fit so
// a lower-priority column never appears without a higher one. With the constants
// here Added lands at width 92 and Took at 100. Whatever flex remains splits
// into a host column clamped to 8–24 and tags taking the rest.
func snapshotLayout(width int) snapLayout {
	const indicator, timeW, sizeW, gaps = 2, 16, 9, 8
	baseFixed := indicator + snapIDWidth + timeW + sizeW + gaps

	var l snapLayout
	reservedExtra := 0
	for _, c := range []struct {
		width int
		on    *bool
	}{
		{snapAddedWidth, &l.showAdded},
		{snapTookWidth, &l.showTook},
	} {
		cost := c.width + 2 // the column plus one more two-space separator
		if width-baseFixed-reservedExtra-cost < snapPromoFlexMin {
			break
		}
		reservedExtra += cost
		*c.on = true
	}

	rest := width - baseFixed - reservedExtra
	if rest < 2 {
		rest = 2
	}
	l.host = rest / 2
	if l.host < 8 {
		l.host = 8
	}
	if l.host > 24 {
		l.host = 24
	}
	l.tags = rest - l.host
	if l.tags < 1 {
		l.tags = 1
	}
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
	rows := 3 // heading, ID, Churn
	if s.Username != "" {
		rows++
	}
	if _, ok := snapshotBackupWindow(*s); ok {
		rows++
	}
	return rows
}

func (m Model) detailOverhead(withSnapDetail, withWindowNote bool) int {
	overhead := headerRows + 2*gapRows + m.footerRows() +
		detailMetaRows + // the seven meta lines
		1 + // the blank line between the meta block and the heading
		1 + // the "Snapshots" heading
		1 // the table's column-header row
	if withWindowNote {
		overhead += 1 // the "showing N–M of T" note
	}
	if withSnapDetail {
		overhead += 1 + // the blank line between the table and the snapshot sub-panel
			m.detailSnapDetailRows() // the selected-snapshot sub-panel
	}
	return overhead
}

// snapshotWindow returns the [start, end) slice bounds for a scrolling window of
// the given size that keeps cursor visible and centered when possible.
func snapshotWindow(cursor, total, size int) (start, end int) {
	if total <= size {
		return 0, total
	}
	start = cursor - size/2
	if start < 0 {
		start = 0
	}
	end = start + size
	if end > total {
		end = total
		start = end - size
	}
	return start, end
}

func bucketLabel(repo config.Repo) string {
	if p := strings.Trim(repo.Path, "/"); p != "" {
		return repo.Bucket + "/" + p
	}
	return repo.Bucket
}

func joinOrDash(vals []string) string {
	if len(vals) == 0 {
		return "—"
	}
	return strings.Join(vals, ", ")
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
