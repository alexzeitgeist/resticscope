package tui

import "charm.land/lipgloss/v2"

// layout.go derives the list view's vertical budget from the captured terminal
// size. The screen, top to bottom, is a one-row header, a blank gap, the repo
// list, a blank gap, and the footer; the list scrolls a window (listWindow) so a
// long repo set never pushes the footer off-screen. Everything here is a pure
// function of the size — nothing reaches into app/model state.

const (
	// defaultWidth/Height stand in before the first WindowSizeMsg (e.g. in
	// tests): wide and tall enough that nothing truncates or scrolls.
	defaultWidth, defaultHeight = 100, 30

	headerRows = 1 // the header line
	gapRows    = 1 // the blank line above and below the list
)

// effSize is the terminal size the layout renders at: the captured size, with
// defaults substituted before the first WindowSizeMsg and a one-cell floor so
// width clipping still honors very small real panes.
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

// footerRows is the rendered footer height (1 normally, 2 while filtering or
// showing a notice). It is measured, not assumed, so the list window shrinks to
// keep the footer flush at the bottom.
func (m Model) footerRows() int { return lipgloss.Height(m.footerView()) }

// listHeight is the number of rows left for the repo list after the header, both
// gaps, and the footer. Floored at 1.
func (m Model) listHeight() int {
	_, h := m.effSize()
	if n := h - headerRows - 2*gapRows - m.footerRows(); n > 1 {
		return n
	}
	return 1
}

// visibleRepos is how many repos the list shows at once: each repo takes two
// lines (the main row plus a meta sub-line) and one line is reserved for the
// "showing N–M of T" note. Floored at 1.
func (m Model) visibleRepos() int {
	if n := (m.listHeight() - 1) / 2; n >= 1 {
		return n
	}
	return 1
}

// listWindow returns the [start, end) bounds of a scrolling window of the given
// visible size that keeps cursor on screen, centering it when possible. It
// mirrors snapshotWindow's contract for the repo list.
func listWindow(cursor, total, visible int) (start, end int) {
	if total <= visible {
		return 0, total
	}
	start = cursor - visible/2
	if start < 0 {
		start = 0
	}
	end = start + visible
	if end > total {
		end = total
		start = end - visible
	}
	return start, end
}

// clip bounds styled or unstyled text to max visible cells on one line without
// breaking ANSI escapes, collapsing any embedded newlines so callers can rely on
// a fixed row budget. It is how every header/footer/list line keeps to its width.
func clip(s string, max int) string {
	if max < 1 {
		max = 1
	}
	return lipgloss.NewStyle().Inline(true).MaxWidth(max).Render(s)
}
