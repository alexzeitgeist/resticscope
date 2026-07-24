package tui

import (
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/theme"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/mattn/go-runewidth"
)

func TestThemeTerminalColors(t *testing.T) {
	light, _ := theme.Lookup("gruvbox-light")
	bg, fg := themeTerminalColors(config.Theme{Name: "gruvbox-light", Background: true})
	if bg != lipgloss.Color(light.Bg) || fg != lipgloss.Color(light.Fg) {
		t.Errorf("terminal colors = %v/%v, want the gruvbox-light bg/fg", bg, fg)
	}

	// Disabling background painting preserves the terminal's own scheme.
	bg, fg = themeTerminalColors(config.Theme{Name: "gruvbox-light", Background: false})
	if bg != nil || fg != nil {
		t.Errorf("background=false must yield nil terminal colors, got %v/%v", bg, fg)
	}
}

func TestTerminalThemeIsUnthemed(t *testing.T) {
	// The terminal theme paints nothing and maps default roles to NoColor.
	if c := paletteColor("default"); c != (lipgloss.NoColor{}) {
		t.Errorf(`paletteColor("default") = %v, want lipgloss.NoColor{}`, c)
	}
	bg, fg := themeTerminalColors(config.Theme{Name: "terminal", Background: true})
	if bg != nil || fg != nil {
		t.Errorf("terminal theme must not paint OSC defaults, got %v/%v", bg, fg)
	}
	term, _ := theme.Lookup("terminal")
	st := newStyles(term)
	// The fixed-width foreground role must emit no color escapes.
	if got := st.name.Render("x"); strings.Contains(got, "\x1b") || strings.TrimRight(got, " ") != "x" {
		t.Errorf("fg=default must render uncolored, got %q", got)
	}
}

func TestThemeTerminalColorsSkipANSI(t *testing.T) {
	// OSC 10/11 require concrete colors; painting ANSI indices would flatten the
	// terminal palette. Hex channels still paint independently.
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

	// Do not force colors before the terminal reports its capabilities.
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

	// A bare quitting view lets the renderer restore terminal defaults.
	m.colorOK = true
	m.quitting = true
	if v := m.View(); v.BackgroundColor != nil || v.ForegroundColor != nil {
		t.Error("quitting view must not carry theme colors")
	}
}

// Status glyphs must share an East Asian width class to keep columns aligned.
// U+2715 has profile (1,1) for narrow/wide ambiguous settings while the shipped
// set is (1,2), so this rejects that regression without pinning glyph literals.
func TestStatusGlyphsShareWidthClass(t *testing.T) {
	narrow := runewidth.NewCondition() // ambiguous counts as 1 cell, lipgloss's default
	wide := runewidth.NewCondition()
	wide.EastAsianWidth = true // ambiguous counts as 2 cells, a CJK-configured terminal

	statuses := []model.Status{
		model.StatusGreen, model.StatusAmber, model.StatusRed,
		model.StatusError, model.StatusGrey,
	}
	type widthProfile struct{ narrow, wide int }
	var want widthProfile
	for i, s := range statuses {
		g := statusGlyph(s)
		runes := []rune(g)
		if len(runes) != 1 {
			t.Fatalf("statusGlyph(%s) = %q, want a single rune", s, g)
		}
		got := widthProfile{narrow.RuneWidth(runes[0]), wide.RuneWidth(runes[0])}
		// The layout reserves one cell under the default width setting.
		if got.narrow != 1 {
			t.Errorf("statusGlyph(%s) = %q spans %d cells (ambiguous=1); status glyphs must be one cell", s, g, got.narrow)
		}
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Errorf("statusGlyph(%s) = %q width profile (ambiguous=1:%d, ambiguous=2:%d) differs from the other glyphs' (%d, %d); a mixed width class breaks column alignment on some terminals",
				s, g, got.narrow, got.wide, want.narrow, want.wide)
		}
	}
}
