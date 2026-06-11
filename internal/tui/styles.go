package tui

import (
	helpbubble "charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"

	"resticscope/internal/model"
)

// styles holds the Lip Gloss styles for the TUI. Colors use the Gruvbox dark
// palette; Lip Gloss degrades gracefully on limited terminals and honors
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

	// Snapshot-diff change-type styles. Each color is a documented Gruvbox hue:
	// green=added, red=removed, yellow=modified, blue/dim=metadata-only,
	// aqua=type-changed, orange+bold=bitrot (loud — it is a corruption signal).
	chgAdded       lipgloss.Style
	chgRemoved     lipgloss.Style
	chgModified    lipgloss.Style
	chgMetadata    lipgloss.Style
	chgTypeChanged lipgloss.Style
	chgBitrot      lipgloss.Style
}

func newStyles() styles {
	var (
		fg     = lipgloss.Color("#ebdbb2")
		grey   = lipgloss.Color("#928374")
		dim    = lipgloss.Color("#7c6f64")
		red    = lipgloss.Color("#fb4934")
		green  = lipgloss.Color("#b8bb26")
		yellow = lipgloss.Color("#fabd2f")
		blue   = lipgloss.Color("#83a598")
		aqua   = lipgloss.Color("#8ec07c")
		orange = lipgloss.Color("#fe8019")
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

// nameWidth is the fixed column width for repo names in the list; labelWidth is
// the fixed column width for field labels in the detail view.
const (
	nameWidth  = 24
	labelWidth = 10
)

// Glyph vocabulary — one glyph per role across every surface (UX plan phase 4):
//
//	·  separates inline values (titles, summaries, status lines, the shell banner)
//	•  separates key chips in the footer bar (the bubbles/help default)
//	—  "no value" in table cells; also the prose dash inside sentences
//	✕  the failure cross (status glyph, extract error headline)
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

// statusGlyph maps a status to its one-cell list glyph (plan §5).
func statusGlyph(s model.Status) string {
	switch s {
	case model.StatusGreen:
		return "●"
	case model.StatusAmber:
		return "▲"
	case model.StatusRed, model.StatusError:
		return "✕"
	default: // grey / never refreshed
		return "…"
	}
}
