package tui

import "charm.land/bubbles/v2/key"

// keyMap is the list- and detail-view keybinding set. It also satisfies
// help.KeyMap via the per-view helpers below, so the footer help reflects what
// the keys do in the current view rather than listing every binding at once.
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	SearchUp   key.Binding // search: move the result cursor up (arrows + ctrl+k, never plain k)
	SearchDown key.Binding // search: move the result cursor down (arrows + ctrl+j, never plain j)
	PageUp     key.Binding
	PageDown   key.Binding
	Enter      key.Binding
	Back       key.Binding
	Shell      key.Binding
	Browse     key.Binding
	Parent     key.Binding // browse: step to the parent directory (backspace/left)
	Open       key.Binding // browse: open the selected directory (right/l), alias for enter
	Refresh    key.Binding
	RefreshAll key.Binding
	Filter     key.Binding
	Search     key.Binding // browse: open the global filename search (same `/` key as Filter)
	Sort       key.Binding
	Help       key.Binding
	Quit       key.Binding // context-aware q: back from nested views, quit on list
	HardQuit   key.Binding // unconditional ctrl+c

	FilterAccept key.Binding
	FilterCancel key.Binding
	FilterDelete key.Binding

	SearchAccept key.Binding
	SearchCancel key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:   key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		// Search-result navigation excludes plain j/k so they stay literal query
		// characters (json, java, kernel, …); arrows and ctrl+j/ctrl+k move instead.
		SearchUp:   key.NewBinding(key.WithKeys("up", "ctrl+k"), key.WithHelp("↑/ctrl+k", "up")),
		SearchDown: key.NewBinding(key.WithKeys("down", "ctrl+j"), key.WithHelp("↓/ctrl+j", "down")),
		PageUp:     key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "page up")),
		PageDown:   key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdn", "page down")),
		Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open/shell")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("q", "back")),
		Shell:      key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "shell")),
		Browse:     key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "browse")),
		Parent:     key.NewBinding(key.WithKeys("backspace", "left", "h"), key.WithHelp("⌫", "parent dir")),
		Open:       key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→", "open")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		RefreshAll: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all")),
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Search:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Sort:       key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "sort")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:       key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		HardQuit:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),

		FilterAccept: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		FilterCancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear")),
		FilterDelete: key.NewBinding(key.WithKeys("backspace")),

		SearchAccept: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		SearchCancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// viewHelp adapts a keyMap to help.KeyMap for the active view: the list shows
// navigation, filter/sort, refresh-all, help, and quit; the detail view swaps in
// the snapshot-scoped keys and back (advertised as q, with esc still bound),
// where q steps back rather than quits; the help overlay shows only back (the
// overlay itself is the full reference). The list keeps Quit because q only exits
// there. While the user is typing a filter (filtering), it shows the apply/clear
// bindings instead. Enter means "open" in the list and "shell here" in the detail
// view (its generic help text covers both).
type viewHelp struct {
	keys      keyMap
	view      view
	filtering bool
	searching bool // browse global filename search input is open
}

func (h viewHelp) ShortHelp() []key.Binding {
	k := h.keys
	if h.filtering {
		return []key.Binding{k.FilterAccept, k.FilterCancel}
	}
	// While the global filename search is open the cursor keys move through the
	// matches and enter/esc open/cancel; the regular browse keys are suspended.
	if h.searching {
		return []key.Binding{k.SearchUp, k.SearchDown, k.SearchAccept, k.SearchCancel}
	}
	switch h.view {
	case detailView:
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Browse, k.Refresh, k.Back}
	case browseView:
		return []key.Binding{k.Up, k.Down, k.Enter, k.Parent, k.Search, k.Sort, k.Shell, k.Back}
	case helpView:
		return []key.Binding{k.Back}
	default: // listView
		return []key.Binding{k.Up, k.Down, k.Enter, k.Shell, k.Refresh, k.Filter, k.Sort, k.Help, k.Quit}
	}
}

func (h viewHelp) FullHelp() [][]key.Binding {
	k := h.keys
	if h.filtering {
		return [][]key.Binding{
			{k.FilterAccept, k.FilterCancel},
		}
	}
	if h.searching {
		return [][]key.Binding{
			{k.SearchUp, k.SearchDown, k.PageUp, k.PageDown},
			{k.SearchAccept, k.SearchCancel},
		}
	}
	switch h.view {
	case detailView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{k.Enter, k.Shell, k.Browse},
			{k.Refresh, k.Back},
		}
	case browseView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{k.Enter, k.Parent, k.Search, k.Sort, k.Shell},
			{k.Back},
		}
	case helpView:
		return [][]key.Binding{
			{k.Back},
		}
	default: // listView
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{k.Enter, k.Shell},
			{k.Refresh, k.RefreshAll},
			{k.Filter, k.Sort},
			{k.Help, k.Quit},
		}
	}
}
