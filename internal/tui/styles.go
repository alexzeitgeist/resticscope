package tui

import (
	"charm.land/lipgloss/v2"

	"resticscope/internal/model"
)

// styles holds the Lip Gloss styles for the list view. Colors are 256-color
// indices that Lip Gloss degrades gracefully on limited terminals, and it
// honors NO_COLOR for free.
type styles struct {
	title    lipgloss.Style
	dim      lipgloss.Style
	name     lipgloss.Style
	selected lipgloss.Style
	meta     lipgloss.Style
	errText  lipgloss.Style
	heading  lipgloss.Style // detail-view section titles
	label    lipgloss.Style // detail-view field labels
	good     lipgloss.Style // positive status text
	bad      lipgloss.Style // negative status text
	glyph    map[model.Status]lipgloss.Style
}

func newStyles() styles {
	var (
		green = lipgloss.Color("42")
		amber = lipgloss.Color("214")
		red   = lipgloss.Color("203")
		grey  = lipgloss.Color("244")
	)
	return styles{
		title:    lipgloss.NewStyle().Bold(true),
		dim:      lipgloss.NewStyle().Foreground(grey),
		name:     lipgloss.NewStyle().Width(nameWidth),
		selected: lipgloss.NewStyle().Bold(true),
		meta:     lipgloss.NewStyle().Foreground(grey),
		errText:  lipgloss.NewStyle().Foreground(red),
		heading:  lipgloss.NewStyle().Bold(true),
		label:    lipgloss.NewStyle().Foreground(grey).Width(labelWidth),
		good:     lipgloss.NewStyle().Foreground(green),
		bad:      lipgloss.NewStyle().Foreground(red),
		glyph: map[model.Status]lipgloss.Style{
			model.StatusGreen: lipgloss.NewStyle().Foreground(green),
			model.StatusAmber: lipgloss.NewStyle().Foreground(amber),
			model.StatusRed:   lipgloss.NewStyle().Foreground(red),
			model.StatusError: lipgloss.NewStyle().Foreground(red),
			model.StatusGrey:  lipgloss.NewStyle().Foreground(grey),
		},
	}
}

// nameWidth is the fixed column width for repo names in the list; labelWidth is
// the fixed column width for field labels in the detail view.
const (
	nameWidth  = 24
	labelWidth = 10
)

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
