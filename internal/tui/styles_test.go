package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"resticscope/internal/config"
	"resticscope/internal/theme"
)

func TestThemeTerminalColors(t *testing.T) {
	light, _ := theme.Lookup("gruvbox-light")
	bg, fg := themeTerminalColors(config.Theme{Name: "gruvbox-light", Background: true})
	if bg != lipgloss.Color(light.Bg) || fg != lipgloss.Color(light.Fg) {
		t.Errorf("terminal colors = %v/%v, want the gruvbox-light bg/fg", bg, fg)
	}

	// background = false opts out of painting entirely: the terminal keeps its
	// own scheme (transparency etc.) and only styled foregrounds are themed.
	bg, fg = themeTerminalColors(config.Theme{Name: "gruvbox-light", Background: false})
	if bg != nil || fg != nil {
		t.Errorf("background=false must yield nil terminal colors, got %v/%v", bg, fg)
	}
}

func TestThemeTerminalColorsSkipANSI(t *testing.T) {
	// OSC 10/11 take a concrete color, not a palette index, so an ANSI-256
	// bg/fg must not be painted (Bubble Tea would flatten it to the fixed
	// xterm table instead of the terminal's own slot). Each channel skips
	// independently; the hex one still paints.
	th := config.Theme{
		Name:       "gruvbox-dark",
		Background: true,
		Colors:     config.ThemeColors{Bg: "0", Fg: "#ebdbb2"},
	}
	bg, fg := themeTerminalColors(th)
	if bg != nil {
		t.Errorf("ANSI bg must not be painted, got %v", bg)
	}
	if fg != lipgloss.Color("#ebdbb2") {
		t.Errorf("hex fg should still paint, got %v", fg)
	}

	th.Colors = config.ThemeColors{Bg: "#101010", Fg: "245"}
	bg, fg = themeTerminalColors(th)
	if bg != lipgloss.Color("#101010") {
		t.Errorf("hex bg should still paint, got %v", bg)
	}
	if fg != nil {
		t.Errorf("ANSI fg must not be painted, got %v", fg)
	}
}

func TestViewPaintsThemeBackground(t *testing.T) {
	a := testApp(nil)
	a.Cfg.Theme = config.Theme{Name: "gruvbox-light", Background: true}
	m := newTestModel(t, a)

	// Before the terminal reports a color profile nothing is painted; a
	// NO_COLOR / dumb terminal must never have its background forced.
	if v := m.View(); v.BackgroundColor != nil || v.ForegroundColor != nil {
		t.Fatal("background painted before the color profile is known")
	}

	up, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m = up.(Model)
	light, _ := theme.Lookup("gruvbox-light")
	v := m.View()
	if v.BackgroundColor != lipgloss.Color(light.Bg) {
		t.Errorf("view background = %v, want gruvbox-light %q", v.BackgroundColor, light.Bg)
	}
	if v.ForegroundColor != lipgloss.Color(light.Fg) {
		t.Errorf("view foreground = %v, want gruvbox-light %q", v.ForegroundColor, light.Fg)
	}

	// An Ascii profile (NO_COLOR) reverts to not painting.
	up, _ = m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = up.(Model)
	if v := m.View(); v.BackgroundColor != nil {
		t.Error("background still painted under an Ascii color profile")
	}

	// Quitting returns a bare view: no colors set, so the renderer resets the
	// terminal to its own defaults on the way out.
	m.colorOK = true
	m.quitting = true
	if v := m.View(); v.BackgroundColor != nil || v.ForegroundColor != nil {
		t.Error("quitting view must not carry theme colors")
	}
}
