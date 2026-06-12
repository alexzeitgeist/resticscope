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
