package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"
)

// The main repository list is a column-aligned table whose optional columns
// disappear as the terminal narrows.

const (
	listGutterWidth   = 2
	listStatusWidth   = 2
	listStatusNameGap = 1
	listLastWidth     = 10
	listSnapsWidth    = 5
	listTookWidth     = 7

	// listLabelsMin keeps the promoted Labels column readable.
	listLabelsMin = 1
)

// rowMeta contains searchable repository metadata plus ordered and keyed label
// representations for display and grouping.
type rowMeta struct {
	region    string
	labels    []string          // Values ordered by key for stable display.
	labelKeys []string          // Keys corresponding to labels.
	byKey     map[string]string // Values keyed for grouping.
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
		slices.Sort(keys)
		rm.labelKeys = keys
		for _, k := range keys {
			rm.labels = append(rm.labels, r.Labels[k])
		}
		out[r.Name] = rm
	}
	return out
}

// View implements tea.Model by rendering the active screen and shared chrome.
func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	_, hasDetail := m.detailRow()
	var title, body string
	switch {
	case m.view == helpView:
		title, body = m.helpTitle(), m.helpBody()
	case m.view == infoView:
		title, body = m.infoTitle(), m.infoBody()
	case m.view == browseView:
		title, body = m.browseTitle(), m.browseBody()
	case m.view == findVersionsView:
		title, body = m.findTitle(), m.findBody()
	case m.view == snapshotDiffView:
		title, body = m.diffTitle(), m.snapshotDiffBody()
	case m.view == diffInfoView:
		title, body = m.diffInfoTitle(), m.diffInfoBody()
	case m.view == extractView:
		title, body = m.styles.title.Render(extractTitle(m.extract)), m.extractBody()
	case m.view == detailView && hasDetail:
		title, body = m.detailTitle(), m.detailBody()
	default:
		title, body = m.listTitle(), m.listView()
	}
	v := tea.NewView(m.frame(title, body))
	v.AltScreen = true
	v.WindowTitle = m.windowTitle()
	// Set terminal colors so light themes do not inherit a dark background.
	if m.colorOK {
		v.BackgroundColor = m.termBg
		v.ForegroundColor = m.termFg
	}
	return v
}

// windowTitle includes the repository for pinned views. Reasserting it on every
// render replaces any title left by a child shell.
func (m Model) windowTitle() string {
	if repo := m.windowTitleRepo(m.view); repo != "" {
		return "resticscope · " + repo
	}
	return "resticscope"
}

// windowTitleRepo returns the repository pinned to v. It does not use the
// cached detailName for the repository-free list view.
func (m Model) windowTitleRepo(v view) string {
	switch v {
	case browseView:
		return m.browseRepo
	case findVersionsView:
		return m.findRepo
	case snapshotDiffView, diffInfoView:
		return m.diffRepo
	case extractView:
		return m.extract.req.Repo
	case detailView, infoView:
		return m.detailName
	case helpView:
		if m.prevView != helpView {
			return m.windowTitleRepo(m.prevView)
		}
	}
	return ""
}

// titleRow places the view title beside the persistent help chip. Text input
// suppresses the chip because ? becomes query text.
func (m Model) titleRow(left string) string {
	w, _ := m.effSize()
	if m.filtering || m.browseSearching || m.diffSearching {
		return clip(left, w)
	}
	right := m.styles.dim.Render("? help")
	gap := 2
	if w <= lipgloss.Width(right)+gap {
		return clip(left, w)
	}
	left = clip(left, w-lipgloss.Width(right)-gap)
	return clip(m.spread(left, right), w)
}

// frame pins the footer below the title and body. Bare newline padding preserves
// body bytes; the terminal clips any overflow.
func (m Model) frame(titleLeft, body string) string {
	_, h := m.effSize()
	footer := m.footerView()
	content := lipgloss.JoinVertical(lipgloss.Left, m.titleRow(titleLeft), "", body)
	blanks := max(h-lipgloss.Height(content)-lipgloss.Height(footer), gapRows)
	return content + strings.Repeat("\n", blanks+1) + footer
}

// listTitle summarizes repository count, restic version, and active controls.
// The live footer prompt replaces the filter indicator during input.
func (m Model) listTitle() string {
	parts := []string{m.styles.title.Render("resticscope")}
	parts = append(parts, m.countLabel())
	if q := normalizedFilter(m.filter); q != "" && !m.filtering {
		parts = append(parts, "filter: "+strconv.Quote(q))
	}
	if m.resticVer != "" {
		parts = append(parts, "restic "+m.resticVer)
	}
	if m.sortMode != sortConfig {
		parts = append(parts, "sort: "+m.sortMode.label())
	}
	if m.groupingActive() {
		parts = append(parts, "group: "+m.activeGroupKey())
	}
	return strings.Join(parts, " · ")
}

// spread places styled strings at opposite ends of one line, using visible
// widths to ignore ANSI escapes.
func (m Model) spread(left, right string) string {
	w, _ := m.effSize()
	gap := "  "
	if n := w - lipgloss.Width(left) - lipgloss.Width(right); n > 2 {
		gap = strings.Repeat(" ", n)
	}
	return left + gap + right
}

// listLayout is the shared variable-column geometry for list headers and rows.
type listLayout struct {
	last, snaps, took, labels int
	showTook, showLabels      bool
}

// computeListLayout always includes Name, Last, and Snaps, then adds Took and
// Labels in that order as space permits.
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

// listCells formats and truncates columns in their shared header/data order.
// Callers join the cells with two spaces.
func listCells(l listLayout, r listRow) []string {
	cells := []string{
		padRight(truncateWidth(r.name, nameWidth), nameWidth),
		fmt.Sprintf("%*s", l.last, truncateWidth(r.last, l.last)),
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

// listHeader uses the row layout and marks name sorting. Config and urgency
// sorting have no matching column, so only the title identifies them.
func listHeader(l listLayout, sort sortMode) string {
	name := "Name"
	if sort == sortName {
		name = markSortColumn(name, false)
	}
	prefix := strings.Repeat(" ", listGutterWidth+listStatusWidth+listStatusNameGap)
	return prefix + strings.Join(listCells(l, listRow{
		name: name, last: "Last", snaps: "Snaps", took: "Took", labels: "Labels",
	}), "  ")
}

// listSection groups repositories under a heading. noKey distinguishes the
// missing-label bucket from real label values.
type listSection struct {
	title string
	rows  []app.RepoStatus
	noKey bool
}

// listDisplay is the canonical render and action order. rows is the flattened
// cursor order; sections is populated only for grouping, ensuring selection and
// rendering cannot diverge.
type listDisplay struct {
	rows     []app.RepoStatus
	sections []listSection
}

// displayList filters and then sorts or groups rows. Its flattened row order is
// also the cursor order.
func (m Model) displayList() listDisplay {
	q := normalizedFilter(m.filter)
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
	sections := groupedSections(filtered, m.meta, m.activeGroupKey(), m.sortMode)
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
	header := clip(m.styles.dim.Render(listHeader(l, m.sortMode)), w)
	if m.groupingActive() {
		return header + "\n" + m.renderGroupedList(d, l, w)
	}
	return header + "\n" + m.renderFlatList(d.rows, l, w)
}

// renderFlatList renders the visible flat-list window and clamps the cursor
// after filtering shrinks the rows.
func (m Model) renderFlatList(rows []app.RepoStatus, l listLayout, width int) string {
	cursor := clampCursor(m.cursor, len(rows))
	start, end := scrollWindow(cursor, len(rows), m.visibleRepos())
	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(rows[i], l, i == cursor, width))
	}
	if start > 0 || end < len(rows) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(rows))), width))
	}
	return strings.Join(lines, "\n")
}

// countLabel returns the total or the filtered "N of M" count. It avoids the
// sort and copy performed by displayList.
func (m Model) countLabel() string {
	total := len(m.rows)
	q := normalizedFilter(m.filter)
	if q == "" {
		return repoCount(total)
	}
	n := 0
	for _, r := range m.rows {
		if matchRepo(r.Name, m.meta[r.Name], q) {
			n++
		}
	}
	return fmt.Sprintf("%d of %s", n, repoCount(total))
}

// renderRow renders one clipped table row. Error and unrefreshed rows replace
// data columns with status text.
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

// statusCell renders a glyph plus a lock or stale marker. Locks take priority;
// stale is hidden during refresh and on errors, where the spinner or failure
// already communicates the repository state.
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
	case row.Stale && !m.pending[row.Name] && row.Status != model.StatusError:
		marker = m.styles.glyph[model.StatusAmber].Render("*")
	}
	return glyph + marker
}

// listRowFor builds display values for a healthy repository; region remains
// searchable but is not displayed.
func (m Model) listRowFor(row app.RepoStatus) listRow {
	return listRow{
		name:   row.Name,
		last:   humanize.Ago(m.app.Clock.Now(), row.State.LastSnapshot),
		snaps:  strconv.Itoa(row.State.SnapshotCount),
		took:   tookDuration(row.State.Snapshots),
		labels: listLabelsValue(m.meta[row.Name], m.activeGroupKey()),
	}
}

// listLabelsValue joins label values, omitting skipKey when its value already
// appears as the group heading.
func listLabelsValue(rm rowMeta, skipKey string) string {
	if skipKey == "" || len(rm.labelKeys) != len(rm.labels) {
		return strings.Join(rm.labels, " · ")
	}
	vals := make([]string, 0, len(rm.labels))
	for i, k := range rm.labelKeys {
		if k == skipKey {
			continue
		}
		vals = append(vals, rm.labels[i])
	}
	return strings.Join(vals, " · ")
}

func tookDuration(snaps []model.Snapshot) string {
	d, ok := model.LastBackupDuration(snaps)
	if !ok {
		return emDash
	}
	return humanize.Duration(d)
}

func (m Model) footerView() string {
	w, _ := m.effSize()
	if m.view == extractView {
		// The extract model supplies per-state bindings and renders status in its
		// body rather than the shared footer.
		return clip(m.help.View(viewHelp{keys: m.keys, view: m.view, extractBindings: m.extract.shortHelp(m.keys)}), w)
	}
	searching := m.browseSearching || m.diffSearching
	help := clip(m.help.View(viewHelp{
		keys:               m.keys,
		view:               m.view,
		filtering:          m.filtering,
		searching:          searching,
		infoScrollable:     m.infoScrollable(),
		diffInfoScrollable: m.diffInfoScrollable(),
		helpScrollable:     m.helpScrollable(),
		searchSuspended:    m.browseSearchSuspended,
		diffJumped:         m.diffSearchJumped,
	}), w)
	switch {
	case m.filtering:
		// Keep / and the live query unstyled so they read as one input token.
		return clip("/"+m.filter+m.styles.dim.Render("▏"), w) + "\n" + help
	case m.browseSearching:
		prompt := "/" + m.browseSearchQuery + m.styles.dim.Render("▏")
		summary := m.browseSearchSummary()
		if m.browseSearchErr != "" {
			return clip(prompt+"  "+m.styles.errText.Render(summary), w) + "\n" + help
		}
		return clip(prompt+m.styles.meta.Render("  "+summary), w) + "\n" + help
	case m.diffSearching:
		prompt := "/" + m.diffSearchQuery + m.styles.dim.Render("▏")
		return clip(prompt+m.styles.meta.Render("  "+m.diffSearchSummary()), w) + "\n" + help
	case m.statusMsg != "":
		return clip(m.styles.errText.Render(m.statusMsg), w) + "\n" + help
	}
	return help
}

func repoCount(n int) string {
	return humanize.Count(n, "repo", "repos")
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}
