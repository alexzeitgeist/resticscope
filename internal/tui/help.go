package tui

import (
	"fmt"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
)

// The help overlay groups keybindings by context and includes the list status
// glyphs. It uses two columns when they fit and a scrollable column otherwise.

type helpEntry struct {
	keys string
	desc string
}

type helpSection struct {
	title   string
	entries []helpEntry
}

// keyLabel renders a binding with the UI's symbols and derives its alternatives
// from the binding so the overlay stays in sync with the handlers.
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

// helpSections returns the reference in single-column reading order.
// Descriptions disambiguate keys that share compact footer labels.
func (m Model) helpSections() []helpSection {
	k := m.keys
	move := keyLabel(k.Up) + " " + keyLabel(k.Down)
	page := keyLabel(k.PageUp) + " " + keyLabel(k.PageDown)
	diffFilters := keyLabel(k.DiffFilterAdded) + " " +
		keyLabel(k.DiffFilterRemoved) + " " +
		keyLabel(k.DiffFilterModified) + " " +
		keyLabel(k.DiffFilterMetadata) + " " +
		keyLabel(k.DiffFilterTypeChanged) + " " +
		keyLabel(k.DiffFilterBitrot)
	// Global excludes view-specific movement and q; ctrl+c is unconditional.
	return []helpSection{
		{"Global", []helpEntry{
			{keyLabel(k.RefreshAll), "refresh all repos"},
			{keyLabel(k.Help), "toggle this help"},
			{keyLabel(k.HardQuit), "quit"},
		}},
		{"List", []helpEntry{
			{move, "move repo cursor"},
			{page, "page up/down"},
			{keyLabel(k.Enter), "open repo detail"},
			{keyLabel(k.Filter), "filter by name/label"},
			{keyLabel(k.Sort), "cycle sort order"},
			{keyLabel(k.Group), "cycle group key"},
			{keyLabel(k.Shell), "shell with repo env"},
			{keyLabel(k.Refresh), "refresh this repo"},
			{keyLabel(k.Quit), "quit"},
		}},
		{"Detail", []helpEntry{
			{move, "select snapshot"},
			{page, "page up/down"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Browse), "browse snapshot files"},
			{keyLabel(k.Info), "snapshot info"},
			{keyLabel(k.Mark), "toggle diff mark"},
			{keyLabel(k.Diff), "open diff for marks"},
			{keyLabel(k.Extract), "extract whole snapshot"},
			{keyLabel(k.Group), "cycle group: host/tags/paths"},
			{keyLabel(k.Collapse), "toggle tree-id collapse"},
			{keyLabel(k.Shell), "shell at snapshot"},
			{keyLabel(k.Refresh), "refresh this repo"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to the list"},
		}},
		{"Browse", []helpEntry{
			{move, "move cursor"},
			{page, "page up/down"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Open), "open directory"},
			{keyLabel(k.Parent), "parent directory"},
			{keyLabel(k.Search), "search filenames"},
			{keyLabel(k.Versions), "file versions across snapshots"},
			{keyLabel(k.Extract), "extract file/dir"},
			{keyLabel(k.Sort), "cycle sort order"},
			{keyLabel(k.Shell), "shell at snapshot"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to detail"},
		}},
		{"Versions", []helpEntry{
			{move, "move cursor"},
			{page, "page up/down"},
			{keyLabel(k.HostToggle), "toggle host filter"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Extract), "extract this version"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to browse"},
		}},
		{"Diff", []helpEntry{
			{move, "move cursor"},
			{page, "page up/down"},
			{keyLabel(k.Enter) + "/" + keyLabel(k.Open), "open directory"},
			{keyLabel(k.Parent), "parent directory"},
			{keyLabel(k.Search), "search changed paths"},
			{keyLabel(k.Extract), "extract changed paths"},
			{keyLabel(k.DiffSwap), "swap snapshot direction"},
			{keyLabel(k.DiffMeta), "include metadata-only changes"},
			{diffFilters, "toggle change filters"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back to detail"},
		}},
		{"Extract", []helpEntry{
			{keyLabel(k.Enter), "extract"},
			{keyLabel(k.Target), "choose target root"},
			{keyLabel(k.Priv), "toggle extract as root"},
			{keyLabel(k.Shell), "shell at extracted dir"},
			{keyLabel(k.Keep) + "/" + keyLabel(k.Delete), "keep / delete staging"},
			{keyLabel(k.Back) + "/" + keyLabel(k.Quit), "back"},
		}},
		{"Filter & search input", []helpEntry{
			{keyLabel(k.FilterAccept), "apply filter / open match"},
			{keyLabel(k.FilterCancel), "clear filter / cancel search"},
			{keyLabel(k.SearchUp) + " " + keyLabel(k.SearchDown), "move through matches"},
			{keyLabel(k.FilterDelete), "delete a character"},
		}},
	}
}

// helpColumns hand-balances the reference for the wide layout. helpBodyLines
// adds the glyph legend to the left column.
func (m Model) helpColumns() (left, right []helpSection) {
	onLeft := map[string]bool{
		"Global": true, "List": true, "Extract": true, "Filter & search input": true,
	}
	for _, s := range m.helpSections() {
		if onLeft[s.title] {
			left = append(left, s)
		} else {
			right = append(right, s)
		}
	}
	return left, right
}

func (m Model) helpTitle() string {
	return m.styles.title.Render("help: keybindings")
}

// helpBody renders the responsive reference window around m.helpScroll.
// Overflowing content gets the same line-range hint as the info modal.
func (m Model) helpBody() string {
	w, _ := m.effSize()
	lines := m.helpBodyLines(w)
	visible := m.modalVisible()
	if len(lines) <= visible {
		return strings.Join(lines, "\n")
	}
	bodyRows := visible - 1
	showHint := bodyRows >= 1
	if !showHint {
		bodyRows = visible
	}
	start := clampModalScroll(m.helpScroll, len(lines), bodyRows)
	end := min(start+bodyRows, len(lines))
	out := make([]string, 0, end-start+1)
	out = append(out, lines[start:end]...)
	if showHint {
		hint := fmt.Sprintf("  showing lines %d–%d of %d", start+1, end, len(lines))
		out = append(out, clip(m.styles.meta.Render(hint), w))
	}
	return strings.Join(out, "\n")
}

// helpBodyLines builds a flat, windowable body. It uses two columns when they
// fit; otherwise it stacks and clips sections to prevent terminal wrapping.
func (m Model) helpBodyLines(width int) []string {
	left, right := m.helpColumns()

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

	twoCol := lipgloss.JoinHorizontal(lipgloss.Top,
		strings.Join(leftBlocks, "\n\n"),
		"      ",
		strings.Join(rightBlocks, "\n\n"),
	)
	if lipgloss.Width(twoCol) <= width {
		return strings.Split(twoCol, "\n")
	}

	var lines []string
	for _, s := range m.helpSections() {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(m.renderHelpSection(s, w), "\n")...)
	}
	lines = append(lines, "")
	lines = append(lines, strings.Split(m.glyphLegend(), "\n")...)
	for i := range lines {
		lines[i] = clip(lines[i], width)
	}
	return lines
}

// scrollHelp adjusts and clamps the overlay scroll offset. It resets the offset
// when the body fits on screen.
func (m Model) scrollHelp(delta int) Model {
	w, _ := m.effSize()
	lines := m.helpBodyLines(w)
	visible := m.modalVisible()
	if len(lines) <= visible {
		m.helpScroll = 0
		return m
	}
	bodyRows := max(visible-1, 1)
	m.helpScroll = clampModalScroll(m.helpScroll+delta, len(lines), bodyRows)
	return m
}

// helpScrollable reports whether the help body overflows the pane. It avoids
// m.footerRows because that builds viewHelp and calls this method; an async
// status message adds a second footer row.
func (m Model) helpScrollable() bool {
	if m.view != helpView {
		return false
	}
	w, h := m.effSize()
	footer := 1
	if m.statusMsg != "" {
		footer = 2
	}
	available := max(h-headerRows-2*gapRows-footer, 1)
	return len(m.helpBodyLines(w)) > available
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

// glyphLegend renders the list status glyphs and trailing markers in their
// display colors. Its marker entries mirror statusCell.
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
