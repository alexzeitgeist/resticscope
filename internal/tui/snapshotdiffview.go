package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/lipgloss/v2"
)

// The diff view renders a directional pair, summary, and directory table.
// clearSnapshotDiff removes its path rows when the view closes.

const (
	// diffMetaRows reserves the path and summary lines above the table.
	diffMetaRows = 2
	// diffAuxRows reserves the table header and optional scroll note.
	diffAuxRows = 2

	// diffMarkerMinWidth fits "+999 -999 M999"; wider markers grow the column.
	diffMarkerMinWidth = 14
	diffNameMin        = 16 // Minimum readable name width.
)

// diffTitle renders the repository and directional pair with compact snapshot
// labels.
func (m Model) diffTitle() string {
	return m.styles.title.Render("diff: " + m.diffRepo + " · " +
		diffSnapshotLabel(m.diffOlder) + " → " + diffSnapshotLabel(m.diffNewer))
}

// diffSnapshotLabel renders a short ID and backup time, falling back to the
// truncated full ID.
func diffSnapshotLabel(s model.Snapshot) string {
	id := s.ShortID
	if id == "" {
		id = shortID(s.ID)
	}
	return id + " " + s.Time.Format("2006-01-02 15:04")
}

// snapshotDiffBody composes the path/summary lines with the table.
func (m Model) snapshotDiffBody() string {
	w, _ := m.effSize()
	pathLine := m.pathLine("Path", browseDirLabel(m.diffDir), w)
	summaryText := m.diffSummaryLine()
	if m.diffSearching {
		summaryText = m.diffSearchSummary()
	}
	summary := clip(m.styles.meta.Render("  "+summaryText), w)
	if m.diffLoading() {
		// Build and show the table only after the stream completes.
		return strings.Join([]string{pathLine, summary}, "\n")
	}
	if m.diffSearching {
		return strings.Join([]string{pathLine, summary, m.diffSearchList(w)}, "\n")
	}
	return strings.Join([]string{pathLine, summary, m.diffList(w)}, "\n")
}

func (m Model) diffSearchSummary() string {
	body := diffSearchSummaryBody(m.diffSearchQuery, m.diffSearchTotal, len(m.diffSearchRows))
	// Keep partial-stream warnings visible when search replaces the summary.
	if m.diffErr != "" {
		return m.diffErr + " · " + body
	}
	return body
}

func diffSearchSummaryBody(query string, total, shown int) string {
	if total == 0 {
		if strings.TrimSpace(query) == "" {
			return searchPrompt
		}
		return noMatchesLabel
	}
	if shown < total {
		return fmt.Sprintf("showing %d of %s", shown, humanize.Count(total, "match", "matches"))
	}
	return humanize.Count(total, "match", "matches")
}

// diffSummaryLine reports progress or statistics, direction, metadata mode, and
// filters. The filter stays last, while a partial warning is prepended without
// replacing useful statistics.
func (m Model) diffSummaryLine() string {
	if m.diffLoading() {
		return fmt.Sprintf("loading… %s seen · esc/back cancels", humanize.Count(m.diffLoadCount, "change", "changes"))
	}
	parts := make([]string, 0, 6)
	if m.diffErr != "" {
		parts = append(parts, m.diffErr)
	}
	parts = append(parts, diffStatsLabel(m.diffStats, m.diffFilters, m.styles))
	parts = append(parts, diffDirectionLegend()...)
	if m.diffMetadata {
		parts = append(parts, "with metadata")
	}
	if m.diffParseErrs > 0 {
		parts = append(parts, diffParseErrorLabel(m.diffParseErrs))
	}
	if m.diffFilters != model.AllDiffKinds {
		parts = append(parts, "filter: "+diffFilterLabel(m.diffFilters))
	}
	return strings.Join(parts, " · ")
}

// diffDirectionLegend uses pair position rather than age because swapping may
// place the older snapshot second.
func diffDirectionLegend() []string {
	return []string{"+ in second snapshot", "- in first snapshot"}
}

// diffStatsLabel renders colored totals and dims excluded kinds without hiding
// their counts.
func diffStatsLabel(s model.DiffStats, filter model.ModifierKind, st styles) string {
	cells := []struct {
		bit   model.ModifierKind
		style lipgloss.Style
		n     int
		glyph string
	}{
		{model.KindAdded, st.chgAdded, s.Added, "+"},
		{model.KindRemoved, st.chgRemoved, s.Removed, "-"},
		{model.KindModified, st.chgModified, s.Modified, "M"},
		{model.KindMetadata, st.chgMetadata, s.MetadataOnly, "U"},
		{model.KindTypeChanged, st.chgTypeChanged, s.TypeChanged, "T"},
		{model.KindBitrot, st.chgBitrot, s.Bitrot, "?"},
	}
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		if c.n == 0 {
			continue
		}
		text := c.glyph + strconv.Itoa(c.n)
		if filter&c.bit == 0 {
			parts = append(parts, st.dim.Render(text))
		} else {
			parts = append(parts, c.style.Render(text))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, " ")
}

// diffFilterLabel renders enabled markers in canonical order.
func diffFilterLabel(k model.ModifierKind) string {
	var b strings.Builder
	if k&model.KindAdded != 0 {
		b.WriteByte('+')
	}
	if k&model.KindRemoved != 0 {
		b.WriteByte('-')
	}
	if k&model.KindModified != 0 {
		b.WriteByte('M')
	}
	if k&model.KindMetadata != 0 {
		b.WriteByte('U')
	}
	if k&model.KindTypeChanged != 0 {
		b.WriteByte('T')
	}
	if k&model.KindBitrot != 0 {
		b.WriteByte('?')
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return b.String()
}

func diffParseErrorLabel(n int) string {
	return humanize.Count(n, "malformed line", "malformed lines") + " ignored"
}

// diffList renders the current directory's windowed table.
func (m Model) diffList(w int) string {
	tw := browseTableWidth(w)
	l := diffLayout(tw, m.diffMarkerCols)
	header := clip(m.styles.dim.Render(diffHeaderRow(l)), tw)
	total := len(m.diffRows)
	if total == 0 {
		return header + "\n" + clip(m.styles.meta.Render("  (no changes in this directory)"), tw)
	}
	cur := clampCursor(m.diffCursor, total)
	start, end := scrollWindow(cur, total, m.diffVisible())
	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		lines = append(lines, m.diffRowView(&m.diffRows[i], i == cur, l, tw))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), tw))
	}
	return strings.Join(lines, "\n")
}

// diffColLayout sizes the marker column to its content while the name flexes.
type diffColLayout struct {
	marker int
	name   int
}

// diffLayout fits the marker column to markerWidth within diffMarkerMinWidth
// and the space left after the minimum name width.
func diffLayout(width, markerWidth int) diffColLayout {
	const indicator, gap = 2, 2
	marker := min(max(markerWidth, diffMarkerMinWidth), max(diffMarkerMinWidth, width-indicator-gap-diffNameMin))
	name := max(width-indicator-marker-gap, diffNameMin)
	return diffColLayout{marker: marker, name: name}
}

// widestDiffMarker measures every row, not just the visible window, so the
// column stays put while scrolling. Directory listings cache the result in
// diffMarkerCols because a directory can hold any number of changed entries.
func (m Model) widestDiffMarker(rows []model.DiffRow) int {
	widest := 0
	for i := range rows {
		own, _, totals := diffRowMarker(&rows[i], m.diffFilters, m.styles)
		widest = max(widest, lipgloss.Width(diffMarkerText(own, totals)))
	}
	return widest
}

func diffHeaderRow(l diffColLayout) string {
	return "  " + fmt.Sprintf("%-*s", l.marker, "Change") + "  " + fmt.Sprintf("%-*s", l.name, "Name")
}

func diffSearchHeaderRow(l diffColLayout) string {
	return "  " + fmt.Sprintf("%-*s", l.marker, "Change") + "  " + fmt.Sprintf("%-*s", l.name, "Path")
}

func (m Model) diffSearchList(w int) string {
	tw := browseTableWidth(w)
	// Search results are capped at diffSearchResultLimit, so measuring them per
	// render stays cheap.
	l := diffLayout(tw, m.widestDiffMarker(m.diffSearchRows))
	header := clip(m.styles.dim.Render(diffSearchHeaderRow(l)), tw)
	total := len(m.diffSearchRows)
	if total == 0 {
		return header
	}
	cur := clampCursor(m.diffSearchCursor, total)
	start, end := scrollWindow(cur, total, m.diffVisible())
	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		lines = append(lines, m.diffRowView(&m.diffSearchRows[i], i == cur, l, tw))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), tw))
	}
	return strings.Join(lines, "\n")
}

// diffRowView renders a colored marker or directory rollup beside a truncated
// name. Selected rows get the accent gutter; clipping preserves one row per
// entry.
func (m Model) diffRowView(r *model.DiffRow, selected bool, l diffColLayout, tw int) string {
	name := r.Name
	if r.IsDir {
		name = "▸ " + name + "/"
	}
	nameCell := padRight(truncatePathWidth(name, l.name), l.name)

	markerCell := m.diffMarkerCell(r, l.marker)

	indicator := "  "
	content := markerCell + "  " + nameCell
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = markerCell + "  " + m.styles.selected.Render(nameCell)
	}
	return clip(indicator+content, tw)
}

// diffMarkerCell truncates and pads the whole marker, then colors only the
// row's own marker so directory totals stay dim.
func (m Model) diffMarkerCell(r *model.DiffRow, width int) string {
	own, ownStyle, totals := diffRowMarker(r, m.diffFilters, m.styles)
	text := padRight(truncateWidth(diffMarkerText(own, totals), width), width)
	if own == "" {
		return m.styles.dim.Render(text)
	}
	// own is a few ASCII markers, well inside diffMarkerMinWidth, so truncation
	// never reaches it.
	return ownStyle.Render(text[:len(own)]) + m.styles.dim.Render(text[len(own):])
}

// diffRowMarker splits a row's marker into its own change marker and style,
// and subtree totals for directories. A directory's own `U` follows the filter
// and sits beside its totals; synthetic ancestors show totals only.
func diffRowMarker(r *model.DiffRow, filter model.ModifierKind, st styles) (own string, ownStyle lipgloss.Style, totals string) {
	if r.IsDir && r.Kinds&model.KindMetadata != 0 && !r.Aggregate.Empty() {
		kinds := r.Kinds & filter
		return model.ModifierString(kinds), diffTypeStyle(model.PrimaryChangeType(kinds), st), diffRollup(r.Aggregate)
	}
	if r.Modifier != "" {
		return r.Modifier, diffTypeStyle(r.Type, st), ""
	}
	if r.IsDir {
		return "", st.dim, diffRollup(r.Aggregate)
	}
	return "?", st.dim, ""
}

// diffMarkerText joins a row's own marker and totals with one space.
func diffMarkerText(own, totals string) string {
	if own == "" || totals == "" {
		return own + totals
	}
	return own + " " + totals
}

// diffRollup renders non-zero aggregate counts with the same canonical markers
// used by summaries, filters, and changed rows.
func diffRollup(s model.DiffStats) string {
	pairs := []struct {
		n     int
		glyph string
	}{
		{s.Added, "+"},
		{s.Removed, "-"},
		{s.Modified, "M"},
		{s.MetadataOnly, "U"},
		{s.TypeChanged, "T"},
		{s.Bitrot, "?"},
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.n > 0 {
			parts = append(parts, p.glyph+strconv.Itoa(p.n))
		}
	}
	if len(parts) == 0 {
		return "·"
	}
	return strings.Join(parts, " ")
}

// diffTypeStyle dims synthetic ancestors so navigation rows do not compete with
// explicit changes.
func diffTypeStyle(t model.ChangeType, st styles) lipgloss.Style {
	switch t {
	case model.ChangeAdded:
		return st.chgAdded
	case model.ChangeRemoved:
		return st.chgRemoved
	case model.ChangeModified:
		return st.chgModified
	case model.ChangeMetadataOnly:
		return st.chgMetadata
	case model.ChangeTypeChanged:
		return st.chgTypeChanged
	case model.ChangeBitrot:
		return st.chgBitrot
	default:
		return st.dim
	}
}
