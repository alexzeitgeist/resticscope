package tui

import (
	"fmt"
	"sort"
	"strings"

	"resticscope/internal/app"
)

// group.go owns the list view's optional grouping by a configured repo label
// key. When `global.group_by` is set, the list partitions repos into sections —
// one per distinct label value, plus a final fallback section for repos
// missing the key. Sort applies within each section so a cycle (e.g. urgency)
// never breaks group boundaries; filter applies before grouping.

// groupingConfigured reports whether the config supplies at least one group_by
// label key. The `g` cycle is a no-op when this is false.
func (m Model) groupingConfigured() bool {
	return len(m.app.Cfg.Global.GroupBy) > 0
}

// groupingActive reports whether the list view should partition by a group_by
// key right now. The transient m.groupIndex cycles via `g` (0 = flat view,
// 1..N = the i-1'th configured key); it never persists.
func (m Model) groupingActive() bool {
	keys := m.app.Cfg.Global.GroupBy
	return m.groupIndex > 0 && m.groupIndex <= len(keys)
}

// activeGroupKey resolves the currently selected group key, or "" while the
// cycle is on the flat-view state (or when groupIndex falls outside the
// configured range, which should never happen but is clamped defensively so
// callers can't see a config-OOB key).
func (m Model) activeGroupKey() string {
	keys := m.app.Cfg.Global.GroupBy
	if m.groupIndex <= 0 || m.groupIndex > len(keys) {
		return ""
	}
	return keys[m.groupIndex-1]
}

// groupedSections partitions filtered rows by their value for key. Sections
// render in case-insensitive order on the group value with the fallback
// section last; sortRows applies within each section so the in-group order
// honors the active sort mode. An empty key value falls into the fallback
// "(no <key>)" bucket, marked with noKey=true so callers can distinguish it
// from a real label value that happens to match the same title.
func groupedSections(rows []app.RepoStatus, meta map[string]rowMeta, key string, mode sortMode) []listSection {
	byValue := make(map[string][]app.RepoStatus, len(rows))
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
	sort.SliceStable(values, func(i, j int) bool {
		li, lj := strings.ToLower(values[i]), strings.ToLower(values[j])
		if li != lj {
			return li < lj
		}
		// Byte-order tie-break for case-only collisions ("Prod" vs "prod").
		// Without it the map iteration order leaks into the rendered order,
		// and two displayList() calls in the same handler can disagree —
		// breaking the canonical render-and-action invariant.
		return values[i] < values[j]
	})
	out := make([]listSection, 0, len(values)+1)
	for _, v := range values {
		s := byValue[v]
		sortRows(s, mode)
		out = append(out, listSection{title: v, rows: s})
	}
	if len(ungrouped) > 0 {
		sortRows(ungrouped, mode)
		out = append(out, listSection{title: "(no " + key + ")", rows: ungrouped, noKey: true})
	}
	return out
}

// groupTok is one rendered line of the flattened grouped-list stream: either a
// data row (data >= 0, indexing into the flat row list) or a heading/blank
// separator (data == -1).
type groupTok struct {
	s    string
	data int
}

// buildGroupTokens flattens sections into a render-ready token stream and
// records each section heading's line index. Blank separators precede every
// non-first section. The cursor row receives the selected highlight.
func (m Model) buildGroupTokens(sections []listSection, cursor int, l listLayout, width int) ([]groupTok, []int) {
	var lines []groupTok
	headingPos := make([]int, len(sections))
	idx := 0
	for si, sec := range sections {
		if si > 0 {
			lines = append(lines, groupTok{s: "", data: -1})
		}
		headingPos[si] = len(lines)
		// Fallback section renders in the dim/meta style so a real label value
		// that happens to match the fallback title can't visually merge with
		// it — the structural noKey flag, not the title string, carries the
		// distinction.
		titleStyle := m.styles.heading
		if sec.noKey {
			titleStyle = m.styles.meta
		}
		title := titleStyle.Render(sec.title) + " " + m.styles.dim.Render(fmt.Sprintf("(%d)", len(sec.rows)))
		lines = append(lines, groupTok{s: clip(title, width), data: -1})
		for _, r := range sec.rows {
			lines = append(lines, groupTok{s: m.renderRow(r, l, idx == cursor, width), data: idx})
			idx++
		}
	}
	return lines, headingPos
}

// groupedWindow picks [start, end) into the flattened token stream so the
// cursor is visible AND its section heading is anchored. Contiguous case
// (cursor fits within max of the heading): window starts at the heading.
// Non-contiguous case: returns prependHeading=true and a tail window of size
// max-1 that never crosses into prior sections. See renderGroupedList for the
// full user-visible contract.
func groupedWindow(cursorPos, hPos, max, n int) (start, end int, prependHeading bool) {
	if cursorPos < hPos+max {
		start = hPos
		end = start + max
		if end > n {
			end = n
		}
		return start, end, false
	}
	prependHeading = true
	tailSize := max - 1
	start = cursorPos - tailSize/2
	if start < hPos+1 {
		start = hPos + 1
	}
	end = start + tailSize
	if end > n {
		end = n
		// No slide-back: once end clamps to n, a full tail window would
		// move start backward to n-tailSize. We deliberately keep the
		// centered start instead, accepting a smaller tail window near EOF.
	}
	return start, end, prependHeading
}

// renderGroupedList paints the grouped sections, windowed to fit the rendered
// line budget (m.listHeight minus the table header and the scroll-note row),
// keeping the selected data row visible. Group headings and blank separators
// between sections consume rendered lines too, so the budget is enforced over
// the full output, not just data rows.
//
// When the whole grouped list fits the budget, sections render top-to-bottom
// in their natural order — no anchoring needed because nothing is truncated.
//
// When the list must be windowed and two or more content lines fit, the
// cursor's group heading is anchored as the first rendered line, never a
// previous section's row or the blank separator above the heading. When the
// cursor sits deep inside a large group and the heading can't appear
// contiguously with the cursor's neighborhood, the heading is emitted alone
// at the top and the tail window below shows rows around the cursor —
// intermediate rows are dropped (the scroll note conveys the truncation).
// When only one content line fits, the selected data row is rendered without
// the heading. The scroll note reports data-row bounds, not section-fragment
// indexes.
func (m Model) renderGroupedList(d listDisplay, l listLayout, width int) string {
	cursor := clampCursor(m.cursor, len(d.rows))
	lines, headingPos := m.buildGroupTokens(d.sections, cursor, l, width)

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

	// Find the heading position for the cursor's section. The flattened row
	// index maps to section i where cursor lies within rowsBefore..rowsBefore+len.
	cursorSec, rowsBefore := 0, 0
	for si, sec := range d.sections {
		if cursor < rowsBefore+len(sec.rows) {
			cursorSec = si
			break
		}
		rowsBefore += len(sec.rows)
	}
	hPos := headingPos[cursorSec]

	start, end, prependHeading := groupedWindow(cursorPos, hPos, max, len(lines))

	out := make([]string, 0, max+1)
	if prependHeading {
		out = append(out, lines[hPos].s)
	}
	for i := start; i < end; i++ {
		out = append(out, lines[i].s)
	}

	// Data-row bounds for the scroll note: scan the rendered window (start..end)
	// for real data tokens. The non-contiguous heading prepended above is itself
	// not a data row, so it doesn't affect the bounds.
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
