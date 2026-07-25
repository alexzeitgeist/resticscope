# Themes

All themes are compiled into the binary, so there is nothing to install.

```toml
[theme]
name = "nord"
```

Available: `ayu-dark`, `ayu-light`, `catppuccin-frappe`, `catppuccin-latte`,
`catppuccin-macchiato`, `catppuccin-mocha`, `dracula`, `gruvbox-dark`,
`gruvbox-light`, `monokai`, `night-owl`, `night-owl-light`, `nord`, `one-dark`,
`solarized-dark`, `solarized-light`, `terminal`, `tokyo-night`.

`gruvbox-dark` is the default. A name that does not exist fails at startup, with
the full list in the error message.

## Following your terminal instead: `terminal`

```toml
[theme]
name = "terminal"
```

This theme hard-codes nothing. Text uses your terminal's default foreground on
its default background, and accents come from the classic ANSI-16 slots: red `1`,
green `2`, yellow `3`, blue `4`, magenta `5` as the accent, cyan `6`, and
bright-black `8` for muted text. resticscope then follows whatever scheme your
terminal is configured with, like the `term16` or `TTY` themes in helix and btop.

## The terminal background: `background`

While the TUI runs, a theme also paints the terminal's default background and
foreground, so `gruvbox-light` really is light even in a dark terminal. Your
colors are restored on exit and whenever resticscope hands the terminal to a
shell (`s`).

```toml
[theme]
background = false
```

Set that to keep your own background, which is what you want with a transparent
terminal or one already themed to match. The background is never painted on
terminals without color support, including `NO_COLOR` sessions.

## Overriding individual colors: `[theme.colors]`

Every theme fills the same ten roles. Override any of them on top of the theme
you chose; override all ten and the result is entirely your own.

```toml
[theme]
name = "gruvbox-dark"

[theme.colors]
orange = "#d65d0e"   # tone down the accent
grey   = "245"       # ANSI-256 slot 245, as your terminal defines it
```

| Role | Used for |
| --- | --- |
| `bg` | the terminal background (see `background` above) |
| `fg` | primary text: names, values |
| `grey` | secondary text: metadata, help descriptions |
| `dim` | separators, disabled rows, truncation |
| `red` | errors, removed paths, overdue repositories |
| `green` | success, added paths, repositories on schedule |
| `yellow` | titles, modified paths, repositories in the grace period |
| `blue` | section headings, directories, metadata-only changes |
| `aqua` | field labels, symlinks, the spinner, type-changed paths |
| `orange` | the accent: cursor row, key hints, bitrot warnings |

Accepted values:

| Form | Example | Meaning |
| --- | --- | --- |
| hex | `"#d65d0e"`, `"#f80"` | that exact color |
| ANSI-256 code | `"245"` | whatever your terminal defines for slot 245 |
| `"default"` | `"default"` | the terminal's default color, rendered with no styling |

Because an ANSI code resolves against your terminal's own palette, a theme built
entirely from ANSI codes follows your terminal scheme automatically. That is how
the `terminal` theme is built.

For `bg` and `fg` specifically, an ANSI code or `"default"` also means that
channel of the background painting is skipped, even with `background = true`: the
escape sequence needs a concrete color, and "your terminal's slot N" is already
what the terminal shows. Only hex values repaint the defaults.

Anything else is rejected at startup, naming the role at fault. Colors degrade
gracefully on limited terminals, and `NO_COLOR` disables them entirely.
