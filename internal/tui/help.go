package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"

	"resticscope/internal/model"
)

// help.go renders the full-screen help overlay (key `?`): a complete keybinding
// reference grouped by the context each key acts in, plus a legend for the list
// status glyphs. The layout is responsive: two balanced columns on a wide
// terminal, a single scrollable column when the width can't fit both. It is
// reached via the helpView value and closed with `?`, `q`,
// or `esc`; the View() switch and handleKey route to it. The title row keeps
// the shared `? help` chip even here — `?` is a toggle, so the affordance stays
// truthful, and dismissal is also advertised in the key bar.

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

// helpSections returns the full reference in single-column reading order:
// the global keys, then each context by navigation depth (list → detail →
// browse → versions/diff), then the extract modal and the text-input keys.
// helpColumns re-splits this list for the wide two-column layout, so the two
// layouts can never drift apart. Entries here use deliberately verbose
// descriptions (e.g. "refresh this repo", "cycle sort order",
// "cycle group key") to disambiguate keys that share a compact footer label
// like "r" or "o"; the binding's WithHelp text drives the one-line footer
// help instead, so the two are not expected to match verbatim.
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
	// Global means shared by the normal list/detail screens; the help overlay is
	// modal and swallows action keys behind it. Cursor movement is not global
	// because it carries view-specific meaning, so it lives under List/Detail.
	// q is not global either: it quits only on the list and steps back from nested
	// views, so it belongs to List, not Global; ctrl+c is the one unconditional quit.
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

// helpColumns splits the reference into the two hand-balanced columns the wide
// layout shows side by side: the contexts entered from the list plus the input
// keys on the left, the drill-down views on the right. The glyph legend joins
// the left column in helpBodyLines.
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

// helpBody renders the reference responsively: two balanced columns when the
// terminal is wide enough, one stacked column when it is not. Either layout is
// windowed around m.helpScroll with a "showing lines" hint when it is taller
// than the pane (the stacked column always is), mirroring infoBody so the two
// modals scroll identically.
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
	end := start + bodyRows
	if end > len(lines) {
		end = len(lines)
	}
	out := make([]string, 0, end-start+1)
	out = append(out, lines[start:end]...)
	if showHint {
		hint := fmt.Sprintf("  showing lines %d–%d of %d", start+1, end, len(lines))
		out = append(out, clip(m.styles.meta.Render(hint), w))
	}
	return strings.Join(out, "\n")
}

// helpBodyLines builds the overlay body as a flat line list so helpBody can
// window it. The two-column layout is used whenever it fits the width; below
// that the sections stack into one column in helpSections' reading order with
// the glyph legend last, every line clipped so a narrow pane truncates a row
// instead of letting the terminal wrap or clip the whole layout.
func (m Model) helpBodyLines(width int) []string {
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

// scrollHelp adjusts the overlay scroll offset by delta lines and clamps the
// result against the actual body extent so the model state always matches what
// the renderer will show. A no-op when the body already fits on screen.
func (m Model) scrollHelp(delta int) Model {
	w, _ := m.effSize()
	lines := m.helpBodyLines(w)
	visible := m.modalVisible()
	if len(lines) <= visible {
		m.helpScroll = 0
		return m
	}
	bodyRows := visible - 1
	if bodyRows < 1 {
		bodyRows = 1
	}
	m.helpScroll = clampModalScroll(m.helpScroll+delta, len(lines), bodyRows)
	return m
}

// helpScrollable reports whether the help overlay's body overflows the visible
// pane and therefore needs to advertise scroll keys in the footer. Like
// infoScrollable it is deliberately independent of m.footerRows() to avoid a
// cycle (footerRows builds viewHelp, which calls this). The overlay cannot have
// an active filter/search prompt, but an async status message can add one
// footer row.
func (m Model) helpScrollable() bool {
	if m.view != helpView {
		return false
	}
	w, h := m.effSize()
	footer := 1
	if m.statusMsg != "" {
		footer = 2
	}
	available := h - headerRows - 2*gapRows - footer
	if available < 1 {
		available = 1
	}
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
