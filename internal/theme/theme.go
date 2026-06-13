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
// roles the TUI uses. Values are "#rgb" / "#rrggbb" hex, an ANSI-256 code
// "0"–"255" (which inherits the terminal's own palette for that slot), or the
// keyword "default" (the terminal's default fg/bg — no color drawn at all,
// the conventional escape hatch tmux/lazygit/helix spell the same way).
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
	// terminal is the no-theming theme: every role defers to the terminal's
	// own scheme. fg/bg are the terminal defaults (nothing painted, nothing
	// styled) and the accents are the classic ANSI-16 slots, so the TUI looks
	// like the rest of the user's terminal — the term16/TTY convention in
	// helix, btop, and friends. Grey and dim share bright-black ("8"), the only
	// muted slot ANSI-16 offers; orange falls to magenta ("5"), the customary
	// stand-in for an accent ANSI-16 lacks.
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
	// monokai is the TextMate original, except Aqua: the .tmTheme has no second
	// cyan, so #a1efe4 follows the common terminal ports (base16) to keep the
	// blue and aqua roles distinguishable rather than collapsing both onto
	// #66d9ef.
	"monokai": {
		Bg: "#272822",
		Fg: "#f8f8f2", Grey: "#75715e", Dim: "#49483e",
		Red: "#f92672", Green: "#a6e22e", Yellow: "#e6db74",
		Blue: "#66d9ef", Aqua: "#a1efe4", Orange: "#fd971f",
	},
	// The four catppuccin flavors share one role mapping onto the official
	// palette (github.com/catppuccin/palette): bg/fg = base/text, grey/dim =
	// overlay0/surface2, and the accents are red/green/yellow/blue/teal/peach.
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
	// ayu follows the upstream ayu-colors syntax roles (markup/string/func/
	// entity/regexp), with orange = keyword rather than ayu's gold accent —
	// the gold (#e6b450 dark, #f29718 light) sits too close to the func yellow
	// already holding the yellow role to keep the two distinguishable.
	"ayu-dark": {
		Bg: "#10141c",
		Fg: "#bfbdb6", Grey: "#5a6673", Dim: "#475266",
		Red: "#f07178", Green: "#aad94c", Yellow: "#ffb454",
		Blue: "#59c2ff", Aqua: "#95e6cb", Orange: "#ff8f40",
	},
	// ayu-light's official comment grey (#adaeb1) is too faint against the
	// near-white bg for the grey role's metadata text, so grey takes the UI
	// foreground and the comment grey slides down to dim. Yellow is the func
	// gold #eba400 darkened to #bf8600: upstream's value reads at 2.1:1 on
	// this bg (the yellow role carries titles, which need more) and ayu has
	// no darker yellow, so this keeps the hue at 3.1:1 — gruvbox-light's
	// yellow weight.
	"ayu-light": {
		Bg: "#fcfcfc",
		Fg: "#5c6166", Grey: "#828e9f", Dim: "#adaeb1",
		Red: "#f07171", Green: "#86b300", Yellow: "#bf8600",
		Blue: "#22a4e6", Aqua: "#4cbf99", Orange: "#fa8532",
	},
	// night-owl maps the VS Code theme's signature tokens: green = the
	// variable chartreuse, yellow = the string tan (the theme's own ANSI
	// yellow is greenish #c5e478, which would collide with green), orange =
	// the number/constant slot. Its iconic keyword purple #c792ea has no role
	// here — the palette carries six accents, Night Owl seven.
	"night-owl": {
		Bg: "#011627",
		Fg: "#d6deeb", Grey: "#637777", Dim: "#1d3b53",
		Red: "#ef5350", Green: "#c5e478", Yellow: "#ecc48d",
		Blue: "#82aaff", Aqua: "#7fdbca", Orange: "#f78c6c",
	},
	// night-owl-light (upstream "Light Owl") has no orange anywhere, so the
	// orange role takes #aa0982 — the number/constant slot, i.e. the same
	// token the dark variant's orange holds — keeping roles consistent when
	// switching between the pair. Dim is the line-number foreground (the
	// whitespace grey #d9d9d9 reads at 1.4:1, invisible as text), and yellow
	// is the official #e0af02 darkened to #b58a00: upstream's only yellows
	// sit at 2:1 on this bg, too faint for the titles the yellow role draws.
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
// "#rrggbb" hex (case-insensitive), an ANSI-256 code 0–255, or the keyword
// "default" (terminal default, rendered unstyled). Apart from the keyword this
// is exactly the set lipgloss.Color parses without falling back to no-color,
// so config validation can reject a typo instead of silently rendering it
// black.
func ValidColor(s string) bool {
	if s == "default" {
		return true
	}
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
	// Reject a leading '+' — strconv.Atoi accepts it, but lipgloss.Color
	// falls back to no-color on "+15", so validation would pass a value
	// that renders as black.
	if len(s) > 0 && s[0] == '+' {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 255
}
