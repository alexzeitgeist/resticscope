package tui

import (
	"image/color"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/theme"

	"charm.land/bubbles/v2/filepicker"
	helpbubble "charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
)

// styles contains theme-derived Lip Gloss styles. Lip Gloss adapts them to
// limited-color and NO_COLOR terminals.
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

	// Diff colors follow palette roles; bitrot is orange and bold to signal
	// corruption.
	chgAdded       lipgloss.Style
	chgRemoved     lipgloss.Style
	chgModified    lipgloss.Style
	chgMetadata    lipgloss.Style
	chgTypeChanged lipgloss.Style
	chgBitrot      lipgloss.Style
}

// paletteColor maps "default" to NoColor so terminal themes and per-role
// overrides can retain the terminal's own color.
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

// filepickerStyles maps the directory-only target picker to the TUI palette.
// DefaultStyles preserves layout values consumed by its renderer.
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

// themeTerminalColors returns concrete OSC 10/11 colors, or nil when background
// painting is disabled. Non-hex values also remain nil because translating an
// ANSI index or "default" would replace the terminal's own palette slot.
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

// nameWidth sizes repository names; labelWidth fits "Repository" plus a space.
const (
	nameWidth  = 24
	labelWidth = 11
)

// Glyph vocabulary: one glyph per role across every surface.

//	·  separates inline values (titles, summaries, status lines, the shell banner)
//	•  separates key chips in the footer bar (the bubbles/help default), and
//	   marks the green/healthy repo in the list status column — two surfaces
//	   that never share a line, so the reuse reads cleanly

//	—  "no value" in table cells; also the prose dash inside sentences
//	×  the failure cross (status glyph, extract error headline)
//	…  truncation and "still loading"

// New text should pick from this table rather than introduce a lookalike.

// statusWord returns the detail title's phrase for a status.
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

// statusGlyph returns one-cell status symbols from the same display-width
// class. The U+00D7 multiplication sign avoids fonts that draw the U+2715
// Dingbat cross double-width.
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
