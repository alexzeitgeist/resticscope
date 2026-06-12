package config

import (
	"strings"
	"testing"

	"resticscope/internal/theme"
)

func TestThemeDefaultsToGruvboxDark(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Theme.Name != theme.DefaultName {
		t.Errorf("theme.name = %q, want %q", cfg.Theme.Name, theme.DefaultName)
	}
	if got, want := cfg.Theme.Palette(), theme.Default(); got != want {
		t.Errorf("default palette = %+v, want %+v", got, want)
	}
}

func TestThemeNameSelectsBuiltin(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[theme]
name = "nord"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, ok := theme.Lookup("nord")
	if !ok {
		t.Fatal("nord is not a built-in theme")
	}
	if got := cfg.Theme.Palette(); got != want {
		t.Errorf("palette = %+v, want nord %+v", got, want)
	}
}

func TestThemeRejectsUnknownName(t *testing.T) {
	_, err := load(t, minimalTOML+`
[theme]
name = "grovbux"
`)
	if err == nil {
		t.Fatal("expected an error for an unknown theme name")
	}
	if !strings.Contains(err.Error(), `theme.name "grovbux"`) {
		t.Errorf("error does not name the bad theme: %v", err)
	}
	// The valid set lives in the binary, so the message must enumerate it.
	if !strings.Contains(err.Error(), theme.DefaultName) {
		t.Errorf("error does not list the available themes: %v", err)
	}
}

func TestThemeColorOverridesApply(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[theme]
name = "nord"

[theme.colors]
orange = "#ff0000"
grey   = "245"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := cfg.Theme.Palette()
	if p.Orange != "#ff0000" {
		t.Errorf("orange override not applied: %q", p.Orange)
	}
	if p.Grey != "245" {
		t.Errorf("ANSI grey override not applied: %q", p.Grey)
	}
	nord, _ := theme.Lookup("nord")
	if p.Blue != nord.Blue {
		t.Errorf("unoverridden blue drifted from the base: %q, want %q", p.Blue, nord.Blue)
	}
}

func TestThemeRejectsExplicitEmptyName(t *testing.T) {
	// Omitted name keeps the default (seeded in Decode); an explicit empty
	// string must overwrite the seed and fail validation like any unknown name
	// rather than being silently corrected.
	_, err := load(t, minimalTOML+`
[theme]
name = ""
`)
	if err == nil || !strings.Contains(err.Error(), `theme.name ""`) {
		t.Fatalf("expected explicit empty theme.name to be rejected, got: %v", err)
	}
}

func TestThemeBackgroundDefaultsTrue(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Theme.Background {
		t.Error("theme.background should default to true when the key is omitted")
	}
}

func TestThemeBackgroundHonorsExplicitFalse(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[theme]
name       = "gruvbox-light"
background = false
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Theme.Background {
		t.Error("explicit theme.background = false was overridden")
	}
}

func TestThemeBgOverrideApplies(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[theme.colors]
bg = "#101010"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p := cfg.Theme.Palette(); p.Bg != "#101010" {
		t.Errorf("bg override not applied: %q", p.Bg)
	}
}

func TestThemeRejectsBadBg(t *testing.T) {
	_, err := load(t, minimalTOML+`
[theme.colors]
bg = "transparent"
`)
	if err == nil || !strings.Contains(err.Error(), "theme.colors.bg") {
		t.Fatalf("expected a theme.colors.bg error, got: %v", err)
	}
}

func TestThemeRejectsBadColor(t *testing.T) {
	_, err := load(t, minimalTOML+`
[theme.colors]
red = "crimson"
`)
	if err == nil {
		t.Fatal("expected an error for a non-hex, non-ANSI color")
	}
	if !strings.Contains(err.Error(), `theme.colors.red`) {
		t.Errorf("error does not name the bad role: %v", err)
	}
}

func TestThemeRejectsUnknownColorRole(t *testing.T) {
	_, err := load(t, minimalTOML+`
[theme.colors]
magenta = "#ff00ff"
`)
	if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
		t.Fatalf("expected an unknown-key error for a bad color role, got: %v", err)
	}
}

func TestThemePaletteFallsBackWithoutNormalize(t *testing.T) {
	// A zero-value Theme (config built in tests, never normalized) must still
	// resolve to the default palette rather than an all-empty one.
	var th Theme
	if got, want := th.Palette(), theme.Default(); got != want {
		t.Errorf("zero-value palette = %+v, want default %+v", got, want)
	}
}
