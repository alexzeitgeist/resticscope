package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"resticscope/internal/model"
)

// snapshotdiffview.go renders snapshotDiffView: a header naming the
// directional first → second snapshot pair, a fixed meta block with the
// breadcrumb and the summary line, and a browse-style table of the current
// dir's children. The on-screen rows hold paths only for the lifetime of the
// model — clearSnapshotDiff zeros every diff* field on leaving the view
// (non-negotiable #1: no filenames linger).

const (
	// diffMetaRows is the number of fixed lines the diff body renders above the
	// scrolling window (the path line and the summary line); diffVisible
	// subtracts it to size the table window.
	diffMetaRows = 2
	// diffAuxRows is the worst-case auxiliary line budget around the table: a
	// column header plus a "showing N–M of T" scroll note. diffVisible reserves
	// both so the footer is never overlapped.
	diffAuxRows = 2

	diffMarkerWidth = 8  // change marker / rollup cell: enough for "+99 ~99" before truncation
	diffNameMin     = 16 // minimum readable Name flex width
)

// snapshotDiffHeaderView renders the title (the repo + directional snapshot pair)
// and the q-back affordance. Both ids are restic short ids so the line stays
// stable at narrow widths; the times use the same compact format browse and
// detail use elsewhere.
func (m Model) snapshotDiffHeaderView() string {
	w, _ := m.effSize()
	label := "diff: " + m.diffRepo + " · " +
		diffSnapshotLabel(m.diffOlder) + " → " + diffSnapshotLabel(m.diffNewer)
	left := m.styles.title.Render(label)
	right := m.styles.dim.Render("q back")
	return clip(m.spread(left, right), w)
}

// diffSnapshotLabel renders one half of the directional pair: the snapshot's
// short id (preferring the canonical ShortID field, falling back to a
// truncated full ID when the cache only carries that) and the backup time.
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
	summary := clip(m.styles.meta.Render("  "+m.diffSummaryLine()), w)
	if m.diffLoading {
		// While the stream is in flight the body area shows only the meta block;
		// the table will appear once entries land and BuildDiffTree runs on the
		// terminal msg.
		return strings.Join([]string{pathLine, summary}, "\n")
	}
	return strings.Join([]string{pathLine, summary, m.diffList(w)}, "\n")
}

// diffSummaryLine is the status sub-line above the table. While loading it
// shows the running entry count plus the cancel affordance; on error it
// surfaces the (path-free) first line; otherwise it reports the top-level
// totals from diffStats, the directional +/− meaning, and the current filter
// mask, plus a hint about the filter keys.
func (m Model) diffSummaryLine() string {
	if m.diffLoading {
		return fmt.Sprintf("loading… %d changes seen · esc/back cancels", m.diffLoadCount)
	}
	if m.diffErr != "" {
		return m.diffErr
	}
	parts := []string{diffStatsLabel(m.diffStats, m.diffFilters, m.styles)}
	parts = append(parts, diffDirectionLegend()...)
	if m.diffParseErrs > 0 {
		parts = append(parts, diffParseErrorLabel(m.diffParseErrs))
	}
	if m.diffFilters != model.AllDiffKinds {
		parts = append(parts, "filter: "+diffFilterLabel(m.diffFilters))
	}
	parts = append(parts, "+/-/M/U/T/b toggle")
	return strings.Join(parts, " · ")
}

func diffDirectionLegend() []string {
	return []string{"+ present in right", "- absent from right"}
}

// diffStatsLabel renders the top-level totals as a compact, colored summary.
// Disabled-kind counters are dimmed so the user can see they are excluded
// without losing the count.
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
		text := c.glyph + fmt.Sprintf("%d", c.n)
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

// diffFilterLabel renders the mask as a sequence of enabled-marker chars, in
// canonical order, so a glance at the summary tells the user which kinds are
// in scope when not all are on.
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
	if n == 1 {
		return "1 malformed line ignored"
	}
	return fmt.Sprintf("%d malformed lines ignored", n)
}

// diffList renders the table window for the current directory: a dim column
// header, the visible rows, and a "showing N–M of T" note when scrolled.
func (m Model) diffList(w int) string {
	tw := browseTableWidth(w)
	l := diffLayout(tw)
	header := clip(m.styles.dim.Render(diffHeaderRow(l)), tw)
	total := len(m.diffRows)
	if total == 0 {
		return header + "\n" + clip(m.styles.meta.Render("  (no changes in this directory)"), tw)
	}
	cur := clampCursor(m.diffCursor, total)
	start, end := snapshotWindow(cur, total, m.diffVisible())
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

// diffColLayout describes the table's variable geometry for a given width.
// Only the Name flex is variable; the marker column is fixed.
type diffColLayout struct {
	marker int
	name   int
}

func diffLayout(width int) diffColLayout {
	const indicator, gap = 2, 2
	baseFixed := indicator + diffMarkerWidth + gap
	name := width - baseFixed
	if name < diffNameMin {
		name = diffNameMin
	}
	return diffColLayout{marker: diffMarkerWidth, name: name}
}

func diffHeaderRow(l diffColLayout) string {
	return "  " + fmt.Sprintf("%-*s", l.marker, "Change") + "  " + fmt.Sprintf("%-*s", l.name, "Name")
}

// diffRowView renders one row. The marker cell shows the row's primary glyph
// (or, for dir rows, a compact `+a -r ~m` rollup) and is colored by the
// primary change type. The name cell shows the last path component, prefixing
// dirs with the same disclosure marker browse uses and keeping the trailing
// slash. The whole line is clipped to width so a long name can't wrap and break
// the row budget; the cursor row is highlighted with the accent gutter and the
// selected style.
func (m Model) diffRowView(r *model.DiffRow, selected bool, l diffColLayout, tw int) string {
	name := r.Name
	if r.IsDir {
		name = "▸ " + name + "/"
	}
	nameCell := padRight(truncateWidth(name, l.name), l.name)

	marker, mstyle := diffRowMarker(r, m.styles)
	markerCell := mstyle.Render(padRight(truncateWidth(marker, l.marker), l.marker))

	indicator := "  "
	content := markerCell + "  " + nameCell
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = markerCell + "  " + m.styles.selected.Render(nameCell)
	}
	return clip(indicator+content, tw)
}

// diffRowMarker chooses what to show in the change-marker cell and which style
// to render it in. Files get the row's raw multi-char `Modifier` so the user
// always sees the full restic vocabulary (e.g. `MU`, `MT`); dirs whose own
// modifier is set behave the same. Synthetic ancestor dirs (Modifier=="",
// Type=ChangeUnknown) fall back to a rollup of their subtree's aggregate.
func diffRowMarker(r *model.DiffRow, st styles) (string, lipgloss.Style) {
	style := diffTypeStyle(r.Type, st)
	if r.Modifier != "" {
		return r.Modifier, style
	}
	if r.IsDir {
		return diffRollup(r.Aggregate), st.dim
	}
	return "?", st.dim
}

// diffRollup is the compact rollup badge for a synthetic ancestor dir: a
// sequence of `<glyph><count>` pairs in canonical order. Only non-zero kinds
// appear; the result is truncated at the marker cell width by the caller.
func diffRollup(s model.DiffStats) string {
	pairs := []struct {
		n     int
		glyph string
	}{
		{s.Added, "+"},
		{s.Removed, "-"},
		{s.Modified, "~"},
		{s.MetadataOnly, "u"},
		{s.TypeChanged, "t"},
		{s.Bitrot, "?"},
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.n > 0 {
			parts = append(parts, p.glyph+fmt.Sprintf("%d", p.n))
		}
	}
	if len(parts) == 0 {
		return "·"
	}
	return strings.Join(parts, " ")
}

// diffTypeStyle returns the change-type style for the row's primary Type.
// ChangeUnknown (synthetic ancestor dirs) falls back to the dim style so a
// navigation-only row never competes with real changes for the user's eye.
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
