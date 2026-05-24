package tui

import (
	"fmt"
	"sort"
	"strings"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// detailMetaRows is the number of fixed meta lines the detail body renders
// (Endpoint/Bucket/Snapshots/Hosts/Tags/Last); detailSnapVisible subtracts it
// from the height to size the scrolling snapshot window.
const detailMetaRows = 6

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
	src := row.State.Snapshots
	snaps := make([]model.Snapshot, len(src))
	copy(snaps, src)
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	return snaps
}

func (m Model) snapCount() int {
	row, _ := m.detailRow()
	return len(row.State.Snapshots)
}

// selectedSnapshot returns the snapshot under the detail cursor, or nil when the
// repo has none. The cursor is clamped on read so a refresh that shrinks the
// list can never index out of range.
func (m Model) selectedSnapshot() *model.Snapshot {
	snaps := m.detailSnapshots()
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
	right := m.styles.dim.Render("b back")
	w, _ := m.effSize()
	return clip(m.spread(left, right), w)
}

func (m Model) detailBody() string {
	row, _ := m.detailRow()
	repo, _ := m.repoConfig(row.Name)
	w, _ := m.effSize()

	sections := []string{
		m.detailMeta(repo, row, w),
		clip(m.styles.heading.Render("Snapshots"), w) + "\n" + m.snapshotTable(),
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
		m.field("Tags", joinOrDash(st.Tags), width),
		m.field("Last", last, width),
	}
	return strings.Join(lines, "\n")
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
// to the terminal width (snapCols) and the window to its height (detailSnapVisible)
// so the table fills the pane without wrapping. Enter on the selection opens a
// shell scoped to it.
func (m Model) snapshotTable() string {
	w, _ := m.effSize()
	hostW, tagsW := snapCols(w)
	header := clip(m.styles.dim.Render(snapHeader(hostW)), w)

	snaps := m.detailSnapshots()
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
		content := fmt.Sprintf("%-16s  %-*s  %9s  %s",
			s.Time.Format("2006-01-02 15:04"),
			hostW, truncate(s.Hostname, hostW),
			size,
			truncate(strings.Join(s.Tags, ","), tagsW),
		)
		indicator := "  "
		if i == cur {
			indicator = m.styles.gutter.Render("▎") + " "
			content = m.styles.selected.Render(content)
		}
		lines = append(lines, clip(indicator+content, w))
	}
	if start > 0 || end < len(snaps) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(snaps))), w))
	}
	return strings.Join(lines, "\n")
}

// snapHeader is the dim column-label row for the snapshot table, aligned to the
// same columns as the data rows: a two-cell indent, fixed time, hostW host, fixed
// size, then tags.
func snapHeader(hostW int) string {
	return fmt.Sprintf("  %-16s  %-*s  %9s  %s", "Time", hostW, "Hostname", "Size", "Tags")
}

// snapCols sizes the snapshot table's variable columns to the total width: a
// two-cell indicator, a 16-cell time and 9-cell size column, and three two-space
// gaps are fixed; the hostname is clamped to a sane range and tags takes the rest.
func snapCols(width int) (host, tags int) {
	const indicator, timeW, sizeW, gaps = 2, 16, 9, 6
	rest := width - indicator - timeW - sizeW - gaps
	if rest < 2 {
		rest = 2
	}
	host = rest / 2
	if host < 8 {
		host = 8
	}
	if host > 24 {
		host = 24
	}
	tags = rest - host
	if tags < 1 {
		tags = 1
	}
	return host, tags
}

// detailSnapVisible is how many snapshot data rows the detail view shows at once:
// the height less the view header, both gaps, the footer, the meta block, the
// blank line and "Snapshots" heading above the table, the table's column header,
// and one line reserved for the "showing N–M of T" note. Floored at 1, which sets
// the detail view's minimum usable height — below it the footer scrolls off.
func (m Model) detailSnapVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() +
		detailMetaRows + // the six meta lines
		1 + // the blank line between the meta block and the heading
		1 + // the "Snapshots" heading
		1 + // the table's column-header row
		1 // the "showing N–M of T" note
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
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
