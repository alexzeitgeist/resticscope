package tui

import (
	"fmt"
	"sort"
	"strings"

	"resticscope/internal/app"
)

// group.go owns the list view's optional grouping by a configured repo label
// key. When `global.group_by` is set, the list partitions repos into sections —
// one per distinct label value, plus a final "Ungrouped" section for repos
// missing the key. Sort applies within each section so a cycle (e.g.
// staleness) never breaks group boundaries; filter applies before grouping.

// groupingConfigured reports whether the config supplies a group_by label key.
// The `g` toggle is a no-op when this is false.
func (m Model) groupingConfigured() bool {
	return m.app.Cfg.Global.GroupBy != ""
}

// groupingActive reports whether the list view should partition by group_by
// right now. The transient m.grouping flag toggles via `g`; it never persists.
func (m Model) groupingActive() bool {
	return m.groupingConfigured() && m.grouping
}

// groupedSections partitions filtered rows by their value for key. Sections
// render in ASCII order on the group value with "Ungrouped" last; sortRows
// applies within each section so the in-group order honors the active sort
// mode. An empty key value falls into the "Ungrouped" bucket.
func groupedSections(rows []app.RepoStatus, meta map[string]rowMeta, key string, mode sortMode) []listSection {
	byValue := make(map[string][]app.RepoStatus)
	var ungrouped []app.RepoStatus
	for _, r := range rows {
		v := meta[r.Name].labelByKey(key)
		if v == "" {
			ungrouped = append(ungrouped, r)
			continue
		}
		byValue[v] = append(byValue[v], r)
	}
	values := make([]string, 0, len(byValue))
	for v := range byValue {
		values = append(values, v)
	}
	sort.Strings(values)
	out := make([]listSection, 0, len(values)+1)
	for _, v := range values {
		s := byValue[v]
		sortRows(s, mode)
		out = append(out, listSection{title: v, rows: s})
	}
	if len(ungrouped) > 0 {
		sortRows(ungrouped, mode)
		out = append(out, listSection{title: "Ungrouped", rows: ungrouped})
	}
	return out
}

// renderGroupedList paints the grouped sections, windowed to fit the rendered
// line budget (m.listHeight minus the table header and the scroll-note row),
// keeping the selected data row visible. Group headings and blank separators
// between sections consume rendered lines too, so the budget is enforced over
// the full output, not just data rows.
//
// If only one content line fits, only the selected data row is rendered (no
// heading); when two or more fit, the row's group heading is included before
// it. The scroll note reports data-row bounds, not section-fragment indexes.
func (m Model) renderGroupedList(d listDisplay, l listLayout, width int) string {
	cursor := clampCursor(m.cursor, len(d.rows))

	type tok struct {
		s    string
		data int // -1 = heading or blank separator
	}
	var lines []tok
	idx := 0
	for si, sec := range d.sections {
		if si > 0 {
			lines = append(lines, tok{s: "", data: -1})
		}
		title := m.styles.heading.Render(sec.title) + " " + m.styles.dim.Render(fmt.Sprintf("(%d)", len(sec.rows)))
		lines = append(lines, tok{s: clip(title, width), data: -1})
		for _, r := range sec.rows {
			lines = append(lines, tok{s: m.renderRow(r, l, idx == cursor, width), data: idx})
			idx++
		}
	}

	max := m.listHeight() - listHeaderRows - listScrollNoteRows
	if max < 1 {
		max = 1
	}

	if max >= len(lines) {
		out := make([]string, len(lines))
		for i, t := range lines {
			out[i] = t.s
		}
		return strings.Join(out, "\n")
	}

	cursorPos := 0
	for i, t := range lines {
		if t.data == cursor {
			cursorPos = i
			break
		}
	}

	// If only one content line fits, render the selected data row alone.
	if max == 1 {
		return lines[cursorPos].s + "\n" + m.scrollNote(cursor, cursor+1, len(d.rows), width)
	}

	start := cursorPos - max/2
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(lines) {
		end = len(lines)
		start = end - max
		if start < 0 {
			start = 0
		}
	}

	dataStart, dataEnd := cursor, cursor+1
	dataStartFound := false
	for i := start; i < end; i++ {
		if lines[i].data >= 0 {
			if !dataStartFound {
				dataStart = lines[i].data
				dataStartFound = true
			}
			dataEnd = lines[i].data + 1
		}
	}

	out := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		out = append(out, lines[i].s)
	}
	if note := m.scrollNote(dataStart, dataEnd, len(d.rows), width); note != "" {
		out = append(out, note)
	}
	return strings.Join(out, "\n")
}

// scrollNote returns the "showing N–M of T" line for a window over a total of
// data rows, or "" when the window covers everything.
func (m Model) scrollNote(start, end, total, width int) string {
	if start <= 0 && end >= total {
		return ""
	}
	return clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), width)
}
