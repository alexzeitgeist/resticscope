package tui

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	helpbubble "charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"

	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/theme"
)

// styles holds the Lip Gloss styles for the TUI. Colors come from the
// configured theme.Palette (gruvbox dark by default; see the [theme] config
// block); Lip Gloss degrades gracefully on limited terminals and honors
// NO_COLOR for free.
type styles struct {
	title        lipgloss.Style
	dim          lipgloss.Style
	name         lipgloss.Style
	selected     lipgloss.Style // detail-view snapshot selection (accent)
	selectedName lipgloss.Style // list cursor row name: accent, fixed-width
	gutter       lipgloss.Style // accent left-gutter glyph on the cursor row
	meta         lipgloss.Style
	key          lipgloss.Style
	errText      lipgloss.Style
	heading      lipgloss.Style // detail-view section titles
	label        lipgloss.Style // detail-view field labels
	extractLabel lipgloss.Style // extract-screen field labels (blue, echoing heading hue)
	good         lipgloss.Style // positive status text
	bad          lipgloss.Style // negative status text
	spinner      lipgloss.Style
	help         helpbubble.Styles
	glyph        map[model.Status]lipgloss.Style

	// Snapshot-diff change-type styles. Each color is a documented palette role:
	// green=added, red=removed, yellow=modified, blue/dim=metadata-only,
	// aqua=type-changed, orange+bold=bitrot (loud — it is a corruption signal).
	chgAdded       lipgloss.Style
	chgRemoved     lipgloss.Style
	chgModified    lipgloss.Style
	chgMetadata    lipgloss.Style
	chgTypeChanged lipgloss.Style
	chgBitrot      lipgloss.Style
}

// paletteColor turns one palette value into a lipgloss color. The keyword
// "default" maps to NoColor — lipgloss then draws no color at all, so the text
// keeps the terminal's own default — which is what lets the built-in
// "terminal" theme (and any per-role "default" override) defer to the
// terminal scheme.
func paletteColor(s string) color.Color {
	if s == "default" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(s)
}

func newStyles(p theme.Palette) styles {
	var (
		fg     = paletteColor(p.Fg)
		grey   = paletteColor(p.Grey)
		dim    = paletteColor(p.Dim)
		red    = paletteColor(p.Red)
		green  = paletteColor(p.Green)
		yellow = paletteColor(p.Yellow)
		blue   = paletteColor(p.Blue)
		aqua   = paletteColor(p.Aqua)
		orange = paletteColor(p.Orange)
	)
	meta := lipgloss.NewStyle().Foreground(grey)
	key := lipgloss.NewStyle().Foreground(orange)
	separator := lipgloss.NewStyle().Foreground(dim)

	return styles{
		title:    lipgloss.NewStyle().Bold(true).Foreground(yellow),
		dim:      separator,
		name:     lipgloss.NewStyle().Foreground(fg).Width(nameWidth),
		selected: lipgloss.NewStyle().Foreground(orange).Bold(true),
		selectedName: lipgloss.NewStyle().
			Width(nameWidth).
			Foreground(orange).
			Bold(true),
		gutter:       lipgloss.NewStyle().Foreground(orange),
		meta:         meta,
		key:          key,
		errText:      lipgloss.NewStyle().Foreground(red),
		heading:      lipgloss.NewStyle().Bold(true).Foreground(blue),
		label:        lipgloss.NewStyle().Foreground(aqua).Width(labelWidth),
		extractLabel: lipgloss.NewStyle().Foreground(blue).Width(labelWidth),
		good:         lipgloss.NewStyle().Foreground(green),
		bad:          lipgloss.NewStyle().Foreground(red),
		spinner:      lipgloss.NewStyle().Foreground(aqua),
		help: helpbubble.Styles{
			ShortKey:       key,
			ShortDesc:      meta,
			ShortSeparator: separator,
			Ellipsis:       separator,
			FullKey:        key,
			FullDesc:       meta,
			FullSeparator:  separator,
		},
		glyph: map[model.Status]lipgloss.Style{
			model.StatusGreen: lipgloss.NewStyle().Foreground(green),
			model.StatusAmber: lipgloss.NewStyle().Foreground(yellow),
			model.StatusRed:   lipgloss.NewStyle().Foreground(red),
			model.StatusError: lipgloss.NewStyle().Foreground(red),
			model.StatusGrey:  lipgloss.NewStyle().Foreground(grey),
		},

		chgAdded:       lipgloss.NewStyle().Foreground(green),
		chgRemoved:     lipgloss.NewStyle().Foreground(red),
		chgModified:    lipgloss.NewStyle().Foreground(yellow),
		chgMetadata:    lipgloss.NewStyle().Foreground(blue),
		chgTypeChanged: lipgloss.NewStyle().Foreground(aqua),
		chgBitrot:      lipgloss.NewStyle().Foreground(orange).Bold(true),
	}
}

// filepickerStyles maps the embedded bubbles filepicker (the extract
// target-root overlay) onto the same palette roles the rest of the TUI uses:
// orange accent for the cursor row, grey for metadata (permissions, sizes),
// dim for the disabled/empty cases. Directories are blue — the navigable,
// selectable rows — while plain files are grey, since this picker only ever
// selects directories. Starting from DefaultStyles keeps the layout-bearing
// bits (the right-aligned size column width, which the picker's cursor-row
// renderer reads back via GetWidth) intact.
func filepickerStyles(p theme.Palette) filepicker.Styles {
	var (
		grey   = paletteColor(p.Grey)
		dim    = paletteColor(p.Dim)
		blue   = paletteColor(p.Blue)
		aqua   = paletteColor(p.Aqua)
		orange = paletteColor(p.Orange)
	)
	s := filepicker.DefaultStyles()
	s.Cursor = lipgloss.NewStyle().Foreground(orange)
	s.Selected = lipgloss.NewStyle().Foreground(orange).Bold(true)
	s.DisabledCursor = lipgloss.NewStyle().Foreground(dim)
	s.DisabledSelected = lipgloss.NewStyle().Foreground(dim)
	s.Directory = lipgloss.NewStyle().Foreground(blue)
	s.File = lipgloss.NewStyle().Foreground(grey)
	s.DisabledFile = lipgloss.NewStyle().Foreground(dim)
	s.Symlink = lipgloss.NewStyle().Foreground(aqua)
	s.Permission = lipgloss.NewStyle().Foreground(grey)
	s.FileSize = s.FileSize.Foreground(grey)
	s.EmptyDirectory = s.EmptyDirectory.Foreground(dim).SetString("empty directory")
	return s
}

// themeTerminalColors resolves the [theme] block to the terminal default
// background/foreground View paints each frame (OSC 11/10). Both are nil —
// paint nothing, keep the terminal's own scheme — when the user opted out via
// `background = false`. A non-hex bg/fg — an ANSI-256 code or the keyword
// "default" (the "terminal" theme) — also skips painting its channel: OSC
// 10/11 take a concrete color, not a palette index, so Bubble Tea would
// flatten an ANSI value to the fixed xterm RGB table instead of the
// terminal's own slot — and for "the terminal's slot N" or "the terminal
// default" the terminal's existing default already is that color, so leaving
// it untouched is the faithful rendering. Every other built-in theme uses hex
// bg/fg and paints normally.
func themeTerminalColors(t config.Theme) (bg, fg color.Color) {
	if !t.Background {
		return nil, nil
	}
	p := t.Palette()
	if strings.HasPrefix(p.Bg, "#") {
		bg = lipgloss.Color(p.Bg)
	}
	if strings.HasPrefix(p.Fg, "#") {
		fg = lipgloss.Color(p.Fg)
	}
	return bg, fg
}

// nameWidth is the fixed column width for repo names in the list; labelWidth is
// the fixed column width for field labels in the detail view — sized to its
// longest label, "Repository", plus one separating space.
const (
	nameWidth  = 24
	labelWidth = 11
)

// Glyph vocabulary — one glyph per role across every surface (UX plan phase 4):
//
//	·  separates inline values (titles, summaries, status lines, the shell banner)
//	•  separates key chips in the footer bar (the bubbles/help default), and
//	   marks the green/healthy repo in the list status column — two surfaces
//	   that never share a line, so the reuse reads cleanly
//	—  "no value" in table cells; also the prose dash inside sentences
//	×  the failure cross (status glyph, extract error headline)
//	…  truncation and "still loading"
//
// New text should pick from this table rather than introduce a lookalike.

// statusWord maps a status to the semantic phrase the detail title shows next
// to the glyph — what the status means, not the literal color name. The help
// legend and list-row texts have their own phrasings and stay independent.
func statusWord(s model.Status) string {
	switch s {
	case model.StatusGreen:
		return "on schedule"
	case model.StatusAmber:
		return "grace period"
	case model.StatusRed:
		return "overdue"
	case model.StatusError:
		return "failed"
	default: // grey
		return "never refreshed"
	}
}

// statusGlyph maps a status to its one-cell list glyph (plan §5). Every glyph
// here must share one terminal-display-width class so the status column lines up
// row to row: the green/amber/grey glyphs are East-Asian "ambiguous" width, so
// the red glyph is `×` (U+00D7, also ambiguous) rather than the heavier Dingbat
// `✕` (U+2715). go-runewidth — and so lipgloss.Width, which sizes the column —
// calls `✕` one cell, but some fonts/terminals draw that Dingbat double-width,
// which shoved every error row one column right of the healthy rows.
func statusGlyph(s model.Status) string {
	switch s {
	case model.StatusGreen:
		return "•"
	case model.StatusAmber:
		return "△"
	case model.StatusRed, model.StatusError:
		return "×"
	default: // grey / never refreshed
		return "…"
	}
}
