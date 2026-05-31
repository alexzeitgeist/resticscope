package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// list.go renders the main repo-list screen: a column-aligned table that
// mirrors the detail snapshot table's polish (a dim header row above
// right/left-aligned cells, graceful promotion as width shrinks). The 5-cell
// prefix in every row — gutter(2) · status cell(2) · status/name gap(1) — sets
// up the Name column's left edge so headers line up with values.

const (
	listGutterWidth   = 2
	listStatusWidth   = 2
	listStatusNameGap = 1
	listLastWidth     = 10
	listSnapsWidth    = 5
	listTookWidth     = 7

	// listLabelsMin keeps Labels promotion (and any Took promotion) from
	// shoving the Labels column below a single readable cell.
	listLabelsMin = 1
)

// rowMeta is the per-repo descriptive data the list view needs: the searchable
// region (no longer shown as a column but still matched by matchRepo), the
// ordered label values for the Labels column, and a key→value map so the
// grouper can resolve a label value by key without re-walking config.
type rowMeta struct {
	region string
	labels []string          // label values, ordered by key for deterministic Labels-column output
	byKey  map[string]string // label key -> value, for grouping lookup
}

// labelByKey resolves a repo's label value for the given key, or "" when the
// key is absent. Nil-safe so test fixtures that don't fill byKey still work.
func (rm rowMeta) labelByKey(key string) string {
	if rm.byKey == nil {
		return ""
	}
	return rm.byKey[key]
}

func buildMeta(cfg *config.Config) map[string]rowMeta {
	out := make(map[string]rowMeta, len(cfg.Repos))
	for _, r := range cfg.Repos {
		rm := rowMeta{region: r.Region, byKey: make(map[string]string, len(r.Labels))}
		keys := make([]string, 0, len(r.Labels))
		for k, v := range r.Labels {
			keys = append(keys, k)
			rm.byKey[k] = v
		}
		sort.Strings(keys)
		for _, k := range keys {
			rm.labels = append(rm.labels, r.Labels[k])
		}
		out[r.Name] = rm
	}
	return out
}

func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	_, hasDetail := m.detailRow()
	var body string
	switch {
	case m.view == helpView:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.helpHeaderView(),
			"",
			m.helpBody(),
		)
	case m.view == browseView:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.browseHeaderView(),
			"",
			m.browseBody(),
		)
	case m.view == detailView && hasDetail:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.detailHeaderView(),
			"",
			m.detailBody(),
		)
	default:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.headerView(),
			"",
			m.listView(),
		)
	}
	content := lipgloss.JoinVertical(lipgloss.Left, body, "", m.footerView())
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// headerView is the top line: the app name, the at-a-glance status badges, the
// repo count, the restic version, and (when active) the sort and group
// indicators. Badges count the visible rows so they agree with countLabel under
// a filter; only the title is bold and only the badges carry color.
func (m Model) headerView() string {
	parts := []string{m.styles.title.Render("resticscope")}
	if badges := m.statusBadges(statusCounts(m.visibleRows())); badges != "" {
		parts = append(parts, badges)
	}
	parts = append(parts, m.countLabel())
	if m.resticVer != "" {
		parts = append(parts, "restic "+m.resticVer)
	}
	if m.sortMode != sortConfig {
		parts = append(parts, "sort: "+m.sortMode.label())
	}
	if m.groupingActive() {
		parts = append(parts, "group: "+m.app.Cfg.Global.GroupBy)
	}
	w, _ := m.effSize()
	return clip(strings.Join(parts, " · "), w)
}

// statusCounts tallies the rows by status across all five buckets.
func statusCounts(rows []app.RepoStatus) map[model.Status]int {
	counts := make(map[model.Status]int, 5)
	for _, r := range rows {
		counts[r.Status]++
	}
	return counts
}

// statusBadges renders the header's count buckets, each in its status color and
// only when non-zero: green (●), amber (▲), failed (✕, red and error summed so ✕
// is never double-counted), and cold (…, grey).
func (m Model) statusBadges(counts map[model.Status]int) string {
	buckets := []struct {
		status model.Status
		n      int
	}{
		{model.StatusGreen, counts[model.StatusGreen]},
		{model.StatusAmber, counts[model.StatusAmber]},
		{model.StatusRed, counts[model.StatusRed] + counts[model.StatusError]},
		{model.StatusGrey, counts[model.StatusGrey]},
	}
	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		parts = append(parts, m.styles.glyph[b.status].Render(fmt.Sprintf("%s%d", statusGlyph(b.status), b.n)))
	}
	return strings.Join(parts, "  ")
}

// spread lays left and right on one line, padding the gap so right sits flush
// against the right edge once the width is known. It expects already styled
// strings: lipgloss.Width discounts the styling escapes. Shared by every view's
// header.
func (m Model) spread(left, right string) string {
	w, _ := m.effSize()
	gap := "  "
	if n := w - lipgloss.Width(left) - lipgloss.Width(right); n > 2 {
		gap = strings.Repeat(" ", n)
	}
	return left + gap + right
}

// listLayout sizes the list table's variable geometry for a given width. The
// header row and every data row share this layout so columns line up at every
// width; listView computes it once and threads it through both renderers,
// mirroring how snapshotLayout is threaded through detail rendering.
type listLayout struct {
	last, snaps, took, labels int
	showTook, showLabels      bool
}

// computeListLayout sizes the list table to width. Name/Last/Snaps are always
// present. Took then Labels promote in strict priority order: Took appears
// first (and may appear alone), and Labels only after Took, taking any flex
// that remains. Strict priority means Labels never appears without Took, so a
// shrinking terminal drops Labels first and then Took, never the other way
// round — and the column identity at a given width is stable as width grows.
func computeListLayout(width int) listLayout {
	l := listLayout{last: listLastWidth, snaps: listSnapsWidth, took: listTookWidth}
	baseFixed := listGutterWidth + listStatusWidth + listStatusNameGap +
		nameWidth + l.last + l.snaps + 2*2 // two 2-space separators between Name/Last and Last/Snaps

	rest := width - baseFixed
	if rest >= listTookWidth+2 {
		l.showTook = true
		rest -= listTookWidth + 2
	}
	if l.showTook && rest >= listLabelsMin+2 {
		l.showLabels = true
		l.labels = rest - 2
	}
	return l
}

// listRow holds one row's raw column values (header or data) for listCells.
type listRow struct {
	name, last, snaps, took, labels string
}

// listCells formats one row's worth of columns into the shared column order so
// the header and data rows always align: Name(24,left) · Last(10,left) ·
// Snaps(5,right) · [Took(7,right)] · [Labels(flex,left)]. Every fixed-width
// value is truncated to its column width before padding so a long value cannot
// widen the row and shove later columns out of alignment. Callers join the
// result with two spaces.
func listCells(l listLayout, r listRow) []string {
	cells := []string{
		padRight(truncateWidth(r.name, nameWidth), nameWidth),
		padRight(truncateWidth(r.last, l.last), l.last),
		fmt.Sprintf("%*s", l.snaps, truncateWidth(r.snaps, l.snaps)),
	}
	if l.showTook {
		cells = append(cells, fmt.Sprintf("%*s", l.took, truncateWidth(r.took, l.took)))
	}
	if l.showLabels {
		cells = append(cells, padRight(truncateWidth(r.labels, l.labels), l.labels))
	}
	return cells
}

// listHeader is the dim column-label row, built from the same listCells layout
// as the data rows plus the 5-cell prefix the rows get from gutter+status+gap,
// so labels line up over their values at every width.
func listHeader(l listLayout) string {
	prefix := strings.Repeat(" ", listGutterWidth+listStatusWidth+listStatusNameGap)
	return prefix + strings.Join(listCells(l, listRow{
		name: "Name", last: "Last", snaps: "Snaps", took: "Took", labels: "Labels",
	}), "  ")
}

// listSection is one group of repos rendered together under a heading. Counts
// derive from len(rows) so there is no duplicate state to keep in sync.
type listSection struct {
	title string
	rows  []app.RepoStatus
}

// listDisplay is the canonical render-and-action order for the list view. rows
// is the flattened selectable order the cursor indexes; sections is non-empty
// only when grouping is active. Rendering and every list-view action resolve
// selection through this single order so the highlighted repo and the acted-on
// repo can never diverge.
type listDisplay struct {
	rows     []app.RepoStatus
	sections []listSection
}

// displayList applies the filter, then either flat sorting or grouped
// partitioning. m.cursor indexes display.rows, not the pre-grouped visibleRows
// order, so cursor navigation in grouped mode steps between data rows in the
// exact order they render on screen.
func (m Model) displayList() listDisplay {
	q := strings.ToLower(strings.TrimSpace(m.filter))
	filtered := make([]app.RepoStatus, 0, len(m.rows))
	for _, r := range m.rows {
		if matchRepo(r.Name, m.meta[r.Name], q) {
			filtered = append(filtered, r)
		}
	}
	if !m.groupingActive() {
		sortRows(filtered, m.sortMode)
		return listDisplay{rows: filtered}
	}
	sections := groupedSections(filtered, m.meta, m.app.Cfg.Global.GroupBy, m.sortMode)
	rows := make([]app.RepoStatus, 0, len(filtered))
	for _, s := range sections {
		rows = append(rows, s.rows...)
	}
	return listDisplay{rows: rows, sections: sections}
}

func (m Model) listView() string {
	w, _ := m.effSize()
	if len(m.rows) == 0 {
		return clip(m.styles.meta.Render("no repositories configured"), w)
	}
	d := m.displayList()
	if len(d.rows) == 0 {
		return clip(m.styles.meta.Render("no repositories match "+strconv.Quote(strings.TrimSpace(m.filter))), w)
	}
	l := computeListLayout(w)
	header := clip(m.styles.dim.Render(listHeader(l)), w)
	if m.groupingActive() {
		return header + "\n" + m.renderGroupedList(d, l, w)
	}
	return header + "\n" + m.renderFlatList(d.rows, l, w)
}

// renderFlatList paints the windowed slice of rows in flat (non-grouped) mode.
// The cursor is clamped here so a filter that shrinks the set can never index
// past the last row.
func (m Model) renderFlatList(rows []app.RepoStatus, l listLayout, width int) string {
	cursor := clampCursor(m.cursor, len(rows))
	start, end := listWindow(cursor, len(rows), m.visibleRepos())
	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(rows[i], l, i == cursor, width))
	}
	if start > 0 || end < len(rows) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(rows))), width))
	}
	return strings.Join(lines, "\n")
}

// countLabel describes how many repos the list is showing: the total normally,
// or "N of M" while a filter narrows the set.
func (m Model) countLabel() string {
	total := len(m.rows)
	if strings.TrimSpace(m.filter) == "" {
		return repoCount(total)
	}
	return fmt.Sprintf("%d of %d repos", len(m.visibleRows()), total)
}

// renderRow renders one repo as a single-line table row. Healthy rows use the
// shared Name/Last/Snaps/Took/Labels cells; error and grey rows put a styled
// message into the space after Name, skipping the numeric columns rather than
// inventing dashes that would look like real data. Every line is clipped to
// width so a long name or refresh error can never wrap and break the
// one-row-per-repo budget.
func (m Model) renderRow(row app.RepoStatus, l listLayout, selected bool, width int) string {
	gutter := "  "
	nameStyle := m.styles.name
	if selected {
		gutter = m.styles.gutter.Render("▎") + " "
		nameStyle = m.styles.selectedName
	}
	prefix := gutter + m.statusCell(row) + " " // 5 cells: gutter(2) + status(2) + gap(1)
	nameCell := nameStyle.Render(padRight(truncateWidth(row.Name, nameWidth), nameWidth))

	switch row.Status {
	case model.StatusError:
		msg := "refresh failed"
		if row.State.LastError != "" {
			msg = "refresh failed: " + firstLine(row.State.LastError)
		}
		return clip(prefix+nameCell+"  "+m.styles.errText.Render(msg), width)
	case model.StatusGrey:
		return clip(prefix+nameCell+"  "+m.styles.meta.Render("never refreshed"), width)
	}

	cells := listCells(l, m.listRowFor(row))
	cells[0] = nameCell
	return clip(prefix+strings.Join(cells, "  "), width)
}

// statusCell renders the 2-cell status area: a colored glyph (or the spinner
// while a refresh is pending) plus a one-cell marker that calls out an active
// lock (`L`) or a stale cache (`*`). Lock wins over stale because it's the more
// actionable signal. The cell width is invariant so column alignment never
// breaks.
func (m Model) statusCell(row app.RepoStatus) string {
	var glyph string
	if m.pending[row.Name] {
		glyph = m.spinner.View()
	} else {
		glyph = m.styles.glyph[row.Status].Render(statusGlyph(row.Status))
	}
	marker := " "
	switch {
	case row.State.LockedSince != nil:
		marker = m.styles.glyph[model.StatusError].Render("L")
	case row.Stale:
		marker = m.styles.glyph[model.StatusAmber].Render("*")
	}
	return glyph + marker
}

// listRowFor builds a listRow with the raw cell values for a healthy repo: the
// humanized "ago" string, snap count, last backup duration, and joined labels.
// Region is excluded — region is still searchable through matchRepo but no
// longer appears as a column.
func (m Model) listRowFor(row app.RepoStatus) listRow {
	return listRow{
		name:   row.Name,
		last:   humanize.Ago(m.app.Clock.Now(), row.State.LastSnapshot),
		snaps:  strconv.Itoa(row.State.SnapshotCount),
		took:   tookDuration(row.State.Snapshots),
		labels: listLabelsValue(m.meta[row.Name]),
	}
}

// listLabelsValue joins a repo's label values for the Labels column with " · "
// separators, matching the rendering style of the old meta sub-line.
func listLabelsValue(rm rowMeta) string {
	return strings.Join(rm.labels, " · ")
}

func tookDuration(snaps []model.Snapshot) string {
	d, ok := model.LastBackupDuration(snaps)
	if !ok {
		return "—"
	}
	return humanize.Duration(d)
}

func (m Model) footerView() string {
	w, _ := m.effSize()
	help := clip(m.help.View(viewHelp{keys: m.keys, view: m.view, filtering: m.filtering, searching: m.browseSearching}), w)
	switch {
	case m.filtering:
		// Show the live query (vim-style) with a block cursor so the input mode
		// is obvious. The "/<query>" stays unstyled so it reads as one token.
		return clip("/"+m.filter+m.styles.dim.Render("▏"), w) + "\n" + help
	case m.browseSearching:
		prompt := "/" + m.browseSearchQuery + m.styles.dim.Render("▏")
		summary := m.browseSearchSummary()
		if m.browseSearchErr != "" {
			return clip(prompt+"  "+m.styles.errText.Render(summary), w) + "\n" + help
		}
		return clip(prompt+m.styles.meta.Render("  "+summary), w) + "\n" + help
	case m.statusMsg != "":
		return clip(m.styles.errText.Render(m.statusMsg), w) + "\n" + help
	}
	return help
}

func repoCount(n int) string {
	if n == 1 {
		return "1 repo"
	}
	return fmt.Sprintf("%d repos", n)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
