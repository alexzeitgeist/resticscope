package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/app"
)

// List grouping partitions filtered repositories by configured label values,
// sorts within sections, and places missing labels in a fallback section.

// groupingConfigured reports whether any grouping key is configured.
func (m Model) groupingConfigured() bool {
	return len(m.app.Cfg.Global.GroupBy) > 0
}

// groupingActive reports whether the transient selector names a configured key.
func (m Model) groupingActive() bool {
	keys := m.app.Cfg.Global.GroupBy
	return m.groupIndex > 0 && m.groupIndex <= len(keys)
}

// activeGroupKey returns the selected key, or empty for flat and invalid selectors.
func (m Model) activeGroupKey() string {
	keys := m.app.Cfg.Global.GroupBy
	if m.groupIndex <= 0 || m.groupIndex > len(keys) {
		return ""
	}
	return keys[m.groupIndex-1]
}

// groupedSections orders label sections case-insensitively, sorts within each,
// and appends a structurally marked missing-label fallback.
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
		// Resolve case-only collisions so map iteration cannot change action order.
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

type groupTokKind int

const (
	groupTokBlank groupTokKind = iota
	groupTokHeading
	groupTokRow
)

// groupTok identifies an unrendered grouped-list line and its local and flat indices.
type groupTok struct {
	kind    groupTokKind
	section int
	row     int
	data    int
}

// buildGroupTokens flattens sections and records heading positions.
func buildGroupTokens(sections []listSection) ([]groupTok, []int) {
	capLines := len(sections)
	if len(sections) > 1 {
		capLines += len(sections) - 1
	}
	for _, sec := range sections {
		capLines += len(sec.rows)
	}
	lines := make([]groupTok, 0, capLines)
	headingPos := make([]int, len(sections))
	idx := 0
	for si, sec := range sections {
		if si > 0 {
			lines = append(lines, groupTok{kind: groupTokBlank, section: si, data: -1})
		}
		headingPos[si] = len(lines)
		lines = append(lines, groupTok{kind: groupTokHeading, section: si, data: -1})
		for ri := range sec.rows {
			lines = append(lines, groupTok{kind: groupTokRow, section: si, row: ri, data: idx})
			idx++
		}
	}
	return lines, headingPos
}

func (m Model) renderGroupToken(t groupTok, sections []listSection, cursor int, l listLayout, width int) string {
	switch t.kind {
	case groupTokBlank:
		return ""
	case groupTokHeading:
		sec := sections[t.section]
		// Style the structural fallback distinctly from an identical real label.
		titleStyle := m.styles.heading
		if sec.noKey {
			titleStyle = m.styles.meta
		}
		title := titleStyle.Render(sec.title) + " " + m.styles.dim.Render(fmt.Sprintf("(%d)", len(sec.rows)))
		return clip(title, width)
	case groupTokRow:
		sec := sections[t.section]
		return m.renderRow(sec.rows[t.row], l, t.data == cursor, width)
	default:
		return ""
	}
}

// groupedWindow keeps the cursor visible with its heading anchored. Deep rows
// use a separate heading plus a tail window that never crosses into prior sections.
func groupedWindow(cursorPos, hPos, max, n int) (start, end int, prependHeading bool) {
	if cursorPos < hPos+max {
		start = hPos
		end = min(start+max, n)
		return start, end, false
	}
	prependHeading = true
	tailSize := max - 1
	start = cursorPos - tailSize/2
	if start < hPos+1 {
		start = hPos + 1
	}
	end = min(start+tailSize,
		// Preserve centering near EOF even when the tail becomes shorter.
		n)
	return start, end, prependHeading
}

// renderGroupedList windows all tokens, including headings and separators, while
// keeping the selected row and its heading visible. A one-line window shows only
// the selected row; scroll notes report data-row rather than token bounds.
func (m Model) renderGroupedList(d listDisplay, l listLayout, width int) string {
	cursor := clampCursor(m.cursor, len(d.rows))
	tokens, headingPos := buildGroupTokens(d.sections)

	max := m.listHeight() - listHeaderRows - listScrollNoteRows
	if max < 1 {
		max = 1
	}

	if max >= len(tokens) {
		out := make([]string, len(tokens))
		for i, t := range tokens {
			out[i] = m.renderGroupToken(t, d.sections, cursor, l, width)
		}
		return strings.Join(out, "\n")
	}

	cursorPos := 0
	for i, t := range tokens {
		if t.kind == groupTokRow && t.data == cursor {
			cursorPos = i
			break
		}
	}

	if max == 1 {
		return m.renderGroupToken(tokens[cursorPos], d.sections, cursor, l, width) + "\n" + m.scrollNote(cursor, cursor+1, len(d.rows), width)
	}

	// Map the flat cursor to its section heading.
	cursorSec, rowsBefore := 0, 0
	for si, sec := range d.sections {
		if cursor < rowsBefore+len(sec.rows) {
			cursorSec = si
			break
		}
		rowsBefore += len(sec.rows)
	}
	hPos := headingPos[cursorSec]

	start, end, prependHeading := groupedWindow(cursorPos, hPos, max, len(tokens))

	out := make([]string, 0, max+1)
	if prependHeading {
		out = append(out, m.renderGroupToken(tokens[hPos], d.sections, cursor, l, width))
	}
	for i := start; i < end; i++ {
		out = append(out, m.renderGroupToken(tokens[i], d.sections, cursor, l, width))
	}

	// Derive scroll bounds from rendered data tokens, excluding headings.
	dataStart, dataEnd := cursor, cursor+1
	dataStartFound := false
	for i := start; i < end; i++ {
		if tokens[i].kind == groupTokRow {
			if !dataStartFound {
				dataStart = tokens[i].data
				dataStartFound = true
			}
			dataEnd = tokens[i].data + 1
		}
	}
	if note := m.scrollNote(dataStart, dataEnd, len(d.rows), width); note != "" {
		out = append(out, note)
	}
	return strings.Join(out, "\n")
}

// scrollNote reports a partial data-row window.
func (m Model) scrollNote(start, end, total, width int) string {
	if start <= 0 && end >= total {
		return ""
	}
	return clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), width)
}
