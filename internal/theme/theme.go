// Package theme defines the TUI's semantic color roles and built-in palettes.
// Colors remain plain strings until internal/tui converts them for lipgloss.
package theme

import (
	"slices"
	"strconv"
)

// Palette assigns colors to the TUI's semantic roles. Values are hex, ANSI-256
// indexes inheriting terminal slots, or "default" for the terminal foreground or
// background.
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

// DefaultName retains the original Gruvbox Dark palette when configuration
// selects none.
const DefaultName = "gruvbox-dark"

// builtin maps upstream palettes onto the ten semantic roles.
var builtin = map[string]Palette{
	// terminal uses terminal defaults and ANSI-16 accents. Bright black supplies
	// both muted roles, while magenta substitutes for the missing orange slot.
	"terminal": {
		Bg: "default", Fg: "default",
		Grey: "8", Dim: "8",
		Red: "1", Green: "2", Yellow: "3",
		Blue: "4", Aqua: "6", Orange: "5",
	},
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
	// Monokai uses the TextMate palette and the base16 secondary cyan to keep Blue
	// and Aqua distinct.
	"monokai": {
		Bg: "#272822",
		Fg: "#f8f8f2", Grey: "#75715e", Dim: "#49483e",
		Red: "#f92672", Green: "#a6e22e", Yellow: "#e6db74",
		Blue: "#66d9ef", Aqua: "#a1efe4", Orange: "#fd971f",
	},
	// Catppuccin flavors map base/text and overlay0/surface2 to structural roles,
	// with official accent colors retained.
	"catppuccin-mocha": {
		Bg: "#1e1e2e",
		Fg: "#cdd6f4", Grey: "#6c7086", Dim: "#585b70",
		Red: "#f38ba8", Green: "#a6e3a1", Yellow: "#f9e2af",
		Blue: "#89b4fa", Aqua: "#94e2d5", Orange: "#fab387",
	},
	"catppuccin-macchiato": {
		Bg: "#24273a",
		Fg: "#cad3f5", Grey: "#6e738d", Dim: "#5b6078",
		Red: "#ed8796", Green: "#a6da95", Yellow: "#eed49f",
		Blue: "#8aadf4", Aqua: "#8bd5ca", Orange: "#f5a97f",
	},
	"catppuccin-frappe": {
		Bg: "#303446",
		Fg: "#c6d0f5", Grey: "#737994", Dim: "#626880",
		Red: "#e78284", Green: "#a6d189", Yellow: "#e5c890",
		Blue: "#8caaee", Aqua: "#81c8be", Orange: "#ef9f76",
	},
	"catppuccin-latte": {
		Bg: "#eff1f5",
		Fg: "#4c4f69", Grey: "#9ca0b0", Dim: "#acb0be",
		Red: "#d20f39", Green: "#40a02b", Yellow: "#df8e1d",
		Blue: "#1e66f5", Aqua: "#179299", Orange: "#fe640b",
	},
	// Ayu uses keyword orange because its gold accent is too close to function
	// yellow for distinct roles.
	"ayu-dark": {
		Bg: "#10141c",
		Fg: "#bfbdb6", Grey: "#5a6673", Dim: "#475266",
		Red: "#f07178", Green: "#aad94c", Yellow: "#ffb454",
		Blue: "#59c2ff", Aqua: "#95e6cb", Orange: "#ff8f40",
	},
	// Ayu Light promotes the UI foreground to Grey because comment grey is too faint
	// for metadata. Yellow is darkened from function gold to improve title contrast.
	"ayu-light": {
		Bg: "#fcfcfc",
		Fg: "#5c6166", Grey: "#828e9f", Dim: "#adaeb1",
		Red: "#f07171", Green: "#86b300", Yellow: "#bf8600",
		Blue: "#22a4e6", Aqua: "#4cbf99", Orange: "#fa8532",
	},
	// Night Owl maps variable chartreuse, string tan, and number orange to avoid a
	// collision between its greenish ANSI yellow and Green. Keyword purple has no
	// semantic role.
	"night-owl": {
		Bg: "#011627",
		Fg: "#d6deeb", Grey: "#637777", Dim: "#1d3b53",
		Red: "#ef5350", Green: "#c5e478", Yellow: "#ecc48d",
		Blue: "#82aaff", Aqua: "#7fdbca", Orange: "#f78c6c",
	},
	// Light Owl uses number/constant magenta for the missing orange role and the
	// line-number foreground for readable Dim text. Yellow is darkened for title
	// contrast.
	"night-owl-light": {
		Bg: "#fbfbfb",
		Fg: "#403f53", Grey: "#989fb1", Dim: "#90a7b2",
		Red: "#de3d3b", Green: "#08916a", Yellow: "#b58a00",
		Blue: "#288ed7", Aqua: "#2aa298", Orange: "#aa0982",
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

// Names returns sorted built-in theme names.
func Names() []string {
	names := make([]string, 0, len(builtin))
	for name := range builtin {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ValidColor reports whether s is a supported config color: hex, an ANSI-256
// index, or "default". These forms are narrower than Lip Gloss's numeric parser.
func ValidColor(s string) bool {
	if s == "default" {
		return true
	}
	if len(s) > 0 && s[0] == '#' {
		hex := s[1:]
		if len(hex) != 3 && len(hex) != 6 {
			return false
		}
		for i := range len(hex) {
			c := hex[i]
			ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !ok {
				return false
			}
		}
		return true
	}
	// Reject a leading plus even though strconv and Lip Gloss accept it.
	if len(s) > 0 && s[0] == '+' {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 255
}
