// Package theme defines the TUI's color vocabulary: the ten semantic roles
// every view draws from, and the built-in named palettes compiled into the
// binary so theme selection ships no extra files. Like internal/model it is a
// leaf — colors are plain strings here (hex or ANSI-256 codes), and only
// internal/tui turns them into lipgloss colors.
package theme

import (
	"slices"
	"strconv"
)

// Palette is one theme's assignment of a color to each of the ten semantic
// roles the TUI uses. Values are strings lipgloss understands: "#rgb" /
// "#rrggbb" hex, or an ANSI-256 code "0"–"255" (which inherits the terminal's
// own palette for that slot).
type Palette struct {
	Bg     string // terminal background the theme is designed on (painted via OSC 11 unless [theme] background = false)
	Fg     string // primary text (names, values)
	Grey   string // secondary text (metadata, help descriptions)
	Dim    string // separators, disabled rows, truncation
	Red    string // errors, removed paths, overdue repos
	Green  string // success, added paths, on-schedule repos
	Yellow string // titles, modified paths, grace-period repos
	Blue   string // section headings, directories, metadata-only changes
	Aqua   string // field labels, symlinks, spinner, type-changed paths
	Orange string // the accent: cursor row, key chips, bitrot warnings
}

// DefaultName is the theme used when the config selects none — the Gruvbox
// dark palette the TUI shipped with originally.
const DefaultName = "gruvbox-dark"

// builtin holds every compiled-in theme, keyed by its config name. Each value
// is the upstream palette mapped onto the ten roles above (bg/fg = the canonical
// background and body text, grey = comment color, dim = the surface/selection
// tone, accents verbatim).
var builtin = map[string]Palette{
	"gruvbox-dark": {
		Bg: "#282828",
		Fg: "#ebdbb2", Grey: "#928374", Dim: "#7c6f64",
		Red: "#fb4934", Green: "#b8bb26", Yellow: "#fabd2f",
		Blue: "#83a598", Aqua: "#8ec07c", Orange: "#fe8019",
	},
	"gruvbox-light": {
		Bg: "#fbf1c7",
		Fg: "#3c3836", Grey: "#928374", Dim: "#a89984",
		Red: "#9d0006", Green: "#79740e", Yellow: "#b57614",
		Blue: "#076678", Aqua: "#427b58", Orange: "#af3a03",
	},
	"nord": {
		Bg: "#2e3440",
		Fg: "#d8dee9", Grey: "#616e88", Dim: "#4c566a",
		Red: "#bf616a", Green: "#a3be8c", Yellow: "#ebcb8b",
		Blue: "#81a1c1", Aqua: "#88c0d0", Orange: "#d08770",
	},
	"dracula": {
		Bg: "#282a36",
		Fg: "#f8f8f2", Grey: "#6272a4", Dim: "#44475a",
		Red: "#ff5555", Green: "#50fa7b", Yellow: "#f1fa8c",
		Blue: "#bd93f9", Aqua: "#8be9fd", Orange: "#ffb86c",
	},
	"monokai": {
		Bg: "#272822",
		Fg: "#f8f8f2", Grey: "#75715e", Dim: "#49483e",
		Red: "#f92672", Green: "#a6e22e", Yellow: "#e6db74",
		Blue: "#66d9ef", Aqua: "#a1efe4", Orange: "#fd971f",
	},
	"catppuccin-mocha": {
		Bg: "#1e1e2e",
		Fg: "#cdd6f4", Grey: "#6c7086", Dim: "#585b70",
		Red: "#f38ba8", Green: "#a6e3a1", Yellow: "#f9e2af",
		Blue: "#89b4fa", Aqua: "#94e2d5", Orange: "#fab387",
	},
	"catppuccin-latte": {
		Bg: "#eff1f5",
		Fg: "#4c4f69", Grey: "#9ca0b0", Dim: "#acb0be",
		Red: "#d20f39", Green: "#40a02b", Yellow: "#df8e1d",
		Blue: "#1e66f5", Aqua: "#179299", Orange: "#fe640b",
	},
	"solarized-dark": {
		Bg: "#002b36",
		Fg: "#839496", Grey: "#657b83", Dim: "#586e75",
		Red: "#dc322f", Green: "#859900", Yellow: "#b58900",
		Blue: "#268bd2", Aqua: "#2aa198", Orange: "#cb4b16",
	},
	"solarized-light": {
		Bg: "#fdf6e3",
		Fg: "#657b83", Grey: "#839496", Dim: "#93a1a1",
		Red: "#dc322f", Green: "#859900", Yellow: "#b58900",
		Blue: "#268bd2", Aqua: "#2aa198", Orange: "#cb4b16",
	},
	"tokyo-night": {
		Bg: "#1a1b26",
		Fg: "#c0caf5", Grey: "#565f89", Dim: "#3b4261",
		Red: "#f7768e", Green: "#9ece6a", Yellow: "#e0af68",
		Blue: "#7aa2f7", Aqua: "#7dcfff", Orange: "#ff9e64",
	},
	"one-dark": {
		Bg: "#282c34",
		Fg: "#abb2bf", Grey: "#5c6370", Dim: "#3e4452",
		Red: "#e06c75", Green: "#98c379", Yellow: "#e5c07b",
		Blue: "#61afef", Aqua: "#56b6c2", Orange: "#d19a66",
	},
}

// Lookup returns the named built-in palette, reporting whether it exists.
func Lookup(name string) (Palette, bool) {
	p, ok := builtin[name]
	return p, ok
}

// Default returns the DefaultName palette.
func Default() Palette {
	return builtin[DefaultName]
}

// Names lists every built-in theme name, sorted, for validation error messages
// and documentation.
func Names() []string {
	names := make([]string, 0, len(builtin))
	for name := range builtin {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ValidColor reports whether s is a color value the TUI accepts: "#rgb" /
// "#rrggbb" hex (case-insensitive) or an ANSI-256 code 0–255. This is exactly
// the set lipgloss.Color parses without falling back to no-color, so config
// validation can reject a typo instead of silently rendering it black.
func ValidColor(s string) bool {
	if len(s) > 0 && s[0] == '#' {
		hex := s[1:]
		if len(hex) != 3 && len(hex) != 6 {
			return false
		}
		for i := 0; i < len(hex); i++ {
			c := hex[i]
			ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !ok {
				return false
			}
		}
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 255
}
