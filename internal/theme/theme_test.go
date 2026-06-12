package theme

import (
	"slices"
	"strconv"
	"testing"
)

func TestDefaultThemeExists(t *testing.T) {
	if _, ok := Lookup(DefaultName); !ok {
		t.Fatalf("default theme %q is not a built-in", DefaultName)
	}
	if Default() != builtin[DefaultName] {
		t.Errorf("Default() does not return the %q palette", DefaultName)
	}
}

func TestDefaultIsOriginalGruvbox(t *testing.T) {
	// The default must stay the exact palette the TUI shipped with, so users
	// who never configure a theme see no change.
	p := Default()
	if p.Bg != "#282828" || p.Fg != "#ebdbb2" || p.Orange != "#fe8019" {
		t.Errorf("default palette drifted from gruvbox dark: %+v", p)
	}
}

func TestNamesSortedAndComplete(t *testing.T) {
	names := Names()
	if len(names) != len(builtin) {
		t.Fatalf("Names() returned %d names, want %d", len(names), len(builtin))
	}
	if !slices.IsSorted(names) {
		t.Errorf("Names() is not sorted: %v", names)
	}
	for _, n := range names {
		if _, ok := Lookup(n); !ok {
			t.Errorf("Names() lists %q but Lookup rejects it", n)
		}
	}
}

// TestTerminalThemeDefersToTerminal pins the no-theming escape hatch: the
// "terminal" theme must not carry a single concrete color — fg/bg are the
// terminal defaults and every accent is an ANSI-16 slot — so the TUI follows
// whatever scheme the terminal itself uses.
func TestTerminalThemeDefersToTerminal(t *testing.T) {
	p, ok := Lookup("terminal")
	if !ok {
		t.Fatal("the terminal theme is not a built-in")
	}
	if p.Bg != "default" || p.Fg != "default" {
		t.Errorf("terminal theme fg/bg must be \"default\", got %q/%q", p.Fg, p.Bg)
	}
	for role, v := range map[string]string{
		"grey": p.Grey, "dim": p.Dim, "red": p.Red, "green": p.Green,
		"yellow": p.Yellow, "blue": p.Blue, "aqua": p.Aqua, "orange": p.Orange,
	} {
		if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 15 {
			t.Errorf("terminal theme role %s = %q, want an ANSI-16 slot", role, v)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("no-such-theme"); ok {
		t.Error("Lookup accepted an unknown theme name")
	}
	if _, ok := Lookup(""); ok {
		t.Error("Lookup accepted the empty name")
	}
}

// TestAllPalettesComplete guards every built-in: a role left empty or
// mistyped would render as no-color at runtime, so it must fail here instead.
func TestAllPalettesComplete(t *testing.T) {
	for name, p := range builtin {
		roles := map[string]string{
			"bg": p.Bg, "fg": p.Fg, "grey": p.Grey, "dim": p.Dim,
			"red": p.Red, "green": p.Green, "yellow": p.Yellow,
			"blue": p.Blue, "aqua": p.Aqua, "orange": p.Orange,
		}
		for role, v := range roles {
			if v == "" {
				t.Errorf("theme %q: role %s is empty", name, role)
			} else if !ValidColor(v) {
				t.Errorf("theme %q: role %s has invalid color %q", name, role, v)
			}
		}
	}
}

func TestValidColor(t *testing.T) {
	valid := []string{"#fff", "#FFF", "#ebdbb2", "#EBDBB2", "#000000", "0", "15", "21", "255", "default"}
	for _, s := range valid {
		if !ValidColor(s) {
			t.Errorf("ValidColor(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "#", "#ff", "#ffff", "#fffff", "#fffffff", "#gggggg", "fff", "ebdbb2", "256", "-1", "1.5", "red", "#ebdbb2 ", "Default", "DEFAULT"}
	for _, s := range invalid {
		if ValidColor(s) {
			t.Errorf("ValidColor(%q) = true, want false", s)
		}
	}
}
