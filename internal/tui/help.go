package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"

	"resticscope/internal/model"
)

// help.go renders the full-screen help overlay (key `?`): a complete keybinding
// reference grouped by the context each key acts in, plus a legend for the list
// status glyphs. It is reached via the helpView value and closed with `?`, `q`,
// or `esc`; the View() switch and handleKey route to it.

// helpEntry is one row of the reference: a key label and what it does.
type helpEntry struct {
	keys string
	desc string
}

// helpSection groups the entries that apply in a single context.
type helpSection struct {
	title   string
	entries []helpEntry
}

// keyLabel renders a binding's keys for the overlay, mapping the raw key names
// to the arrow/symbol forms used elsewhere in the UI and joining alternates with
// "/". Deriving the label from the binding (rather than hardcoding it) keeps the
// overlay in step with the keys the handlers actually match.
func keyLabel(b key.Binding) string {
	repl := map[string]string{
		"up": "↑", "down": "↓", "left": "←", "right": "→", "backspace": "⌫",
	}
	keys := b.Keys()
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if sym, ok := repl[k]; ok {
			k = sym
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, "/")
}

// helpColumns returns the reference split into two balanced columns so the whole
// overlay fits a standard terminal without scrolling. The glyph legend is added
// to the left column by helpBody. Entries here use deliberately verbose
// descriptions (e.g. "refresh this repo", "cycle sort order",
// "cycle group key") to disambiguate keys that share a compact footer label
// like "r" or "o"; the binding's WithHelp text drives the one-line footer
// help instead, so the two are not expected to match verbatim.
func (m Model) helpColumns() (left, right []helpSection) {
	k := m.keys
	move := keyLabel(k.Up) + " " + keyLabel(k.Down)
	page := keyLabel(k.PageUp) + " " + keyLabel(k.PageDown)
	// Global means shared by the normal list/detail screens; the help overlay is
	// modal and swallows action keys behind it. Cursor movement is not global
	// because it carries view-specific meaning, so it lives under List/Detail.
	// q is not global either: it quits only on the list and steps back from nested
	// views, so it belongs to List, not Global; ctrl+c is the one unconditional quit.
	left = []helpSection{
		{"Global", []helpEntry{
			{keyLabel(k.RefreshAll), "refresh all repos"},
			{keyLabel(k.Help), "toggle this help"},
			{keyLabel(k.HardQuit), "quit"},
		}},
		{"List", []helpEntry{
			{move, "move repo cursor"},
			{page, "page up/down"},
			{keyLabel(k.Enter), "open repo detail"},
			{keyLabel(k.Shell), "shell with repo env"},
			{keyLabel(k.Refresh), "refresh this repo"},
			{keyLabel(k.Filter), "filter by name/label"},
			{keyLabel(k.Sort), "cycle sort order"},
			{keyLabel(k.Group), "cycle group key"},
			{keyLabel(k.Quit), "quit"},
		}},
	}
	right = []helpSection{
		{"Detail", []helpEntry{
			{move, "select snapshot"},
			{page, "page up/down"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Browse), "browse snapshot files"},
			{keyLabel(k.Shell), "shell at snapshot"},
			{keyLabel(k.Refresh), "refresh this repo"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to the list"},
		}},
		{"Browse", []helpEntry{
			{move, "move cursor"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Open), "open directory"},
			{keyLabel(k.Parent), "parent directory"},
			{keyLabel(k.Search), "search filenames"},
			{keyLabel(k.Versions), "file versions across snapshots"},
			{keyLabel(k.Sort), "cycle sort order"},
			{keyLabel(k.Shell), "shell at snapshot"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to detail"},
		}},
		{"Versions", []helpEntry{
			{move, "move cursor"},
			{keyLabel(k.HostToggle), "toggle host filter"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to browse"},
		}},
		{"Filter", []helpEntry{
			{keyLabel(k.FilterAccept), "apply filter"},
			{keyLabel(k.FilterCancel), "clear filter"},
			{keyLabel(k.FilterDelete), "delete a character"},
		}},
	}
	return left, right
}

func (m Model) helpHeaderView() string {
	w, _ := m.effSize()
	return clip(m.spread(
		m.styles.title.Render("resticscope · keybindings"),
		m.styles.dim.Render("? close"),
	), w)
}

func (m Model) helpBody() string {
	left, right := m.helpColumns()

	// One key column width across both columns keeps the descriptions aligned.
	w := helpKeyWidth(append(append([]helpSection{}, left...), right...))

	leftBlocks := make([]string, 0, len(left)+1)
	for _, s := range left {
		leftBlocks = append(leftBlocks, m.renderHelpSection(s, w))
	}
	leftBlocks = append(leftBlocks, m.glyphLegend())

	rightBlocks := make([]string, 0, len(right))
	for _, s := range right {
		rightBlocks = append(rightBlocks, m.renderHelpSection(s, w))
	}

	return lipgloss.JoinHorizontal(lipgloss.Top,
		strings.Join(leftBlocks, "\n\n"),
		"      ",
		strings.Join(rightBlocks, "\n\n"),
	)
}

func helpKeyWidth(secs []helpSection) int {
	w := 0
	for _, s := range secs {
		for _, e := range s.entries {
			if n := lipgloss.Width(e.keys); n > w {
				w = n
			}
		}
	}
	return w
}

func (m Model) renderHelpSection(s helpSection, keyWidth int) string {
	lines := make([]string, 0, len(s.entries)+1)
	lines = append(lines, m.styles.heading.Render(s.title))
	for _, e := range s.entries {
		lines = append(lines, "  "+m.styles.key.Render(padRight(e.keys, keyWidth))+"  "+m.styles.meta.Render(e.desc))
	}
	return strings.Join(lines, "\n")
}

// glyphLegend explains the list view's status glyphs and the trailing marker
// cell, rendered in their own colors so the legend matches what the list shows.
// The marker entries mirror statusCell's render logic so the user can decode
// the `L` and `*` they see in the second status column.
func (m Model) glyphLegend() string {
	items := []struct {
		status model.Status
		desc   string
	}{
		{model.StatusGreen, "within expected window"},
		{model.StatusAmber, "in the grace period"},
		{model.StatusRed, "overdue or failed"},
		{model.StatusGrey, "never refreshed"},
	}
	lines := make([]string, 0, len(items)+3)
	lines = append(lines, m.styles.heading.Render("Status"))
	for _, it := range items {
		glyph := m.styles.glyph[it.status].Render(statusGlyph(it.status))
		lines = append(lines, "  "+glyph+"  "+m.styles.meta.Render(it.desc))
	}
	lockMarker := m.styles.glyph[model.StatusError].Render("L")
	staleMarker := m.styles.glyph[model.StatusAmber].Render("*")
	lines = append(lines,
		"  "+lockMarker+"  "+m.styles.meta.Render("repository is locked"),
		"  "+staleMarker+"  "+m.styles.meta.Render("cached data is stale"),
	)
	return strings.Join(lines, "\n")
}

// padRight pads s with spaces to a visible width of w (a no-op if already wider),
// using lipgloss.Width so multi-cell runes line up.
func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
