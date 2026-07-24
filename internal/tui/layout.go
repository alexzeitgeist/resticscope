package tui

import "charm.land/lipgloss/v2"

// Layout helpers derive responsive row budgets and scrolling windows from the
// terminal size without consulting application state.

const (
	// defaultWidth and defaultHeight apply before the first WindowSizeMsg.
	defaultWidth, defaultHeight = 100, 30

	headerRows         = 1 // Title row.
	gapRows            = 1 // Blank line below the title and above the footer.
	listHeaderRows     = 1 // Table header.
	listScrollNoteRows = 1 // Reserved "showing N-M of T" row.
)

// effSize returns the captured terminal size, applying startup defaults and a
// one-cell floor for small panes.
func (m Model) effSize() (w, h int) {
	w, h = m.width, m.height
	if w <= 0 {
		w = defaultWidth
	}
	if h <= 0 {
		h = defaultHeight
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// footerRows measures the rendered footer so content leaves exactly enough
// space to keep it pinned at the bottom.
func (m Model) footerRows() int { return lipgloss.Height(m.footerView()) }

// modalVisible returns the modal body rows left after the header, gaps, and
// footer, with a floor of one.
func (m Model) modalVisible() int {
	_, h := m.effSize()
	if n := h - headerRows - 2*gapRows - m.footerRows(); n >= 1 {
		return n
	}
	return 1
}

// clampModalScroll bounds a (possibly out-of-range) modal scroll offset so the
// body window [start, start+bodyRows) stays inside [0, total).
func clampModalScroll(scroll, total, bodyRows int) int {
	maxScroll := max(total-bodyRows, 0)
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	return scroll
}

// listHeight returns the rows left after the header, gaps, and footer.
func (m Model) listHeight() int {
	_, h := m.effSize()
	if n := h - headerRows - 2*gapRows - m.footerRows(); n > 1 {
		return n
	}
	return 1
}

// visibleRepos returns the flat-list page size after reserving the table header
// and scroll note. Grouped rendering uses the full line budget instead.
func (m Model) visibleRepos() int {
	if n := m.listHeight() - listHeaderRows - listScrollNoteRows; n >= 1 {
		return n
	}
	return 1
}

// clampCursor bounds a cursor to the list, returning zero for an empty list.
func clampCursor(i, total int) int {
	if i >= total {
		i = total - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

// scrollWindow returns a centered [start, end) window that keeps cursor visible.
// All scrolling lists use it.
func scrollWindow(cursor, total, visible int) (start, end int) {
	if visible < 1 {
		visible = 1
	}
	if total <= visible {
		return 0, total
	}
	start = max(cursor-visible/2, 0)
	end = start + visible
	if end > total {
		end = total
		start = end - visible
	}
	return start, end
}

// clip bounds text to max visible cells without breaking ANSI escapes and
// collapses embedded newlines to preserve fixed row budgets.
func clip(s string, max int) string {
	if max < 1 {
		max = 1
	}
	return lipgloss.NewStyle().Inline(true).MaxWidth(max).Render(s)
}
