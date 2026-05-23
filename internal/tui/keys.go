package tui

import "charm.land/bubbles/v2/key"

// keyMap is the list- and detail-view keybinding set. It also satisfies
// help.KeyMap via the per-view helpers below, so the footer help reflects what
// the keys do in the current view rather than listing every binding at once.
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	Enter      key.Binding
	Back       key.Binding
	Shell      key.Binding
	Refresh    key.Binding
	RefreshAll key.Binding
	Coverage   key.Binding
	Help       key.Binding
	Quit       key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open/shell")),
		Back:       key.NewBinding(key.WithKeys("b", "esc"), key.WithHelp("b", "back")),
		Shell:      key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "shell")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		RefreshAll: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all")),
		Coverage:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "coverage")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// viewHelp adapts a keyMap to help.KeyMap for the active view: the list shows
// navigation, coverage, refresh-all, and help; the detail view swaps in back
// and the snapshot-scoped keys; the coverage view shows only back and
// refresh-all. Enter means "open" in the list and "shell here" in the detail
// view (its generic help text covers both).
type viewHelp struct {
	keys keyMap
	view view
}

func (h viewHelp) ShortHelp() []key.Binding {
	k := h.keys
	switch h.view {
	case detailView:
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Refresh, k.Back, k.Quit}
	case coverageView:
		return []key.Binding{k.RefreshAll, k.Back, k.Quit}
	default: // listView
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Refresh, k.Coverage, k.Help, k.Quit}
	}
}

func (h viewHelp) FullHelp() [][]key.Binding {
	k := h.keys
	switch h.view {
	case detailView:
		return [][]key.Binding{
			{k.Up, k.Down},
			{k.Enter, k.Shell},
			{k.Refresh, k.Back, k.Quit},
		}
	case coverageView:
		return [][]key.Binding{
			{k.RefreshAll, k.Back, k.Quit},
		}
	default: // listView
		return [][]key.Binding{
			{k.Up, k.Down},
			{k.Enter, k.Shell},
			{k.Refresh, k.RefreshAll, k.Coverage},
			{k.Help, k.Quit},
		}
	}
}
