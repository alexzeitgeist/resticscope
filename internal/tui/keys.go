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
	Filter     key.Binding
	Sort       key.Binding
	Help       key.Binding
	Quit       key.Binding

	// Filter-input-mode bindings. They are matched only while the user is typing
	// a filter (m.filtering), so they may safely reuse keys like enter and esc
	// that mean something else in the normal list view.
	FilterAccept key.Binding
	FilterCancel key.Binding
	FilterDelete key.Binding
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
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Sort:       key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "sort")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

		FilterAccept: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		FilterCancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear")),
		FilterDelete: key.NewBinding(key.WithKeys("backspace")),
	}
}

// viewHelp adapts a keyMap to help.KeyMap for the active view: the list shows
// navigation, filter/sort, coverage, refresh-all, and help; the detail view
// swaps in back and the snapshot-scoped keys; the coverage view shows only back
// and refresh-all; the help overlay shows only back and quit (the overlay itself
// is the full reference). While the user is typing a filter (filtering), it shows
// the apply/clear bindings instead. Enter means "open" in the list and "shell
// here" in the detail view (its generic help text covers both).
type viewHelp struct {
	keys      keyMap
	view      view
	filtering bool
}

func (h viewHelp) ShortHelp() []key.Binding {
	k := h.keys
	if h.filtering {
		return []key.Binding{k.FilterAccept, k.FilterCancel}
	}
	switch h.view {
	case detailView:
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Refresh, k.Back, k.Quit}
	case coverageView:
		return []key.Binding{k.RefreshAll, k.Back, k.Quit}
	case helpView:
		return []key.Binding{k.Back, k.Quit}
	default: // listView
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Refresh, k.Filter, k.Sort, k.Coverage, k.Help, k.Quit}
	}
}

func (h viewHelp) FullHelp() [][]key.Binding {
	k := h.keys
	if h.filtering {
		return [][]key.Binding{
			{k.FilterAccept, k.FilterCancel},
		}
	}
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
	case helpView:
		return [][]key.Binding{
			{k.Back, k.Quit},
		}
	default: // listView
		return [][]key.Binding{
			{k.Up, k.Down},
			{k.Enter, k.Shell},
			{k.Refresh, k.RefreshAll, k.Coverage},
			{k.Filter, k.Sort},
			{k.Help, k.Quit},
		}
	}
}
