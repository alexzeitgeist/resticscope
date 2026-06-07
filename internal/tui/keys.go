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
	Group      key.Binding // list: cycle group_by keys + flat view; detail: cycle snapshot section grouping (off → host → tags → paths)
	Collapse   key.Binding // detail: toggle tree-ID collapse on the snapshot table (folds consecutive same-tree rows into "(+N)")
	Versions   key.Binding // browse: open the find-versions view for the selected file
	HostToggle key.Binding // find-versions: toggle the host filter on/off
	Mark       key.Binding // detail: toggle the cursor snapshot's place in the 2-slot diff FIFO
	Diff       key.Binding // detail: open the diff view for the resolved (older, newer) pair
	Info       key.Binding // detail: open the full snapshot-info modal
	DiffSwap   key.Binding // diff: swap the directional first/second pair and rerun
	Extract    key.Binding // browse: open the extract modal for the selected entry (step 07)
	ExtractGo  key.Binding // extract: commit the extract (review→running or preview→running)
	Target     key.Binding // extract: open the target-root filepicker overlay from review
	Keep       key.Binding // extract: keep the staging dir from the cancel/error prompt
	Delete     key.Binding // extract: delete the staging dir from the cancel/error prompt
	Help       key.Binding
	Quit       key.Binding // context-aware q: back from nested views, quit on list
	HardQuit   key.Binding // unconditional ctrl+c

	// Snapshot-diff filter toggles: one bit each in model.ModifierKind. `?`
	// collides with Help (the modifier char for bitrot), so the binding is `b`.
	DiffFilterAdded       key.Binding
	DiffFilterRemoved     key.Binding
	DiffFilterModified    key.Binding
	DiffFilterMetadata    key.Binding
	DiffFilterTypeChanged key.Binding
	DiffFilterBitrot      key.Binding

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
		Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
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
		Group:      key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "cycle group")),
		Collapse:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "collapse")),
		Versions:   key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "versions")),
		HostToggle: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "all hosts")),
		Mark:       key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle mark")),
		Diff:       key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "diff")),
		Info:       key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info")),
		DiffSwap:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "swap")),
		Extract:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "extract")),
		ExtractGo:  key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "go")),
		Target:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "target")),
		Keep:       key.NewBinding(key.WithKeys("k"), key.WithHelp("k", "keep")),
		Delete:     key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:       key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		HardQuit:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),

		DiffFilterAdded:       key.NewBinding(key.WithKeys("+"), key.WithHelp("+", "added")),
		DiffFilterRemoved:     key.NewBinding(key.WithKeys("-"), key.WithHelp("-", "removed")),
		DiffFilterModified:    key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "modified")),
		DiffFilterMetadata:    key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "metadata")),
		DiffFilterTypeChanged: key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "type")),
		DiffFilterBitrot:      key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "bitrot")),

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
// bindings instead. Enter does something different in each view (open detail in
// the list, browse the selected snapshot in detail, open directory in browse),
// so its footer label is overridden per view via enterAs below.
type viewHelp struct {
	keys           keyMap
	view           view
	filtering      bool
	searching      bool // browse global filename search input is open
	infoScrollable bool // info modal body overflows; advertise up/down in the footer
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
		return []key.Binding{k.Up, k.Down, enterAs(k, "browse"), k.Mark, k.Diff, k.Info, k.Group, k.Collapse, k.Shell, k.Refresh, k.Back}
	case browseView:
		// The compact footer is width-bound, and browse already fills it. Collapse
		// the two cursor-movement bindings into one "↑/↓ move" entry so the extract
		// action fits alongside the existing browse keys without clipping
		// shell/back on an ~100-col terminal. j/k still move (the ? overlay lists
		// them); only the footer hint is condensed.
		move := key.NewBinding(key.WithKeys("up", "down", "j", "k"), key.WithHelp("↑/↓", "move"))
		return []key.Binding{move, enterAs(k, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell, k.Back}
	case findVersionsView:
		return []key.Binding{k.Up, k.Down, k.HostToggle, k.Back}
	case snapshotDiffView:
		return []key.Binding{k.Up, k.Down, enterAs(k, "open"), k.Parent, k.Search, k.DiffSwap, k.DiffFilterAdded, k.Back}
	case extractView:
		// Extract renders its own per-state footer via extract.helpLine in
		// footerView, bypassing this bubble-help path entirely. This branch is a
		// fallback only; it advertises the always-present back affordance.
		return []key.Binding{k.Back}
	case helpView:
		return []key.Binding{k.Back}
	case infoView:
		// `i` already advertised itself in the modal header (the "i close"
		// hint), so the footer carries only the canonical back key — plus the
		// scroll keys when the body overflows.
		if h.infoScrollable {
			return []key.Binding{k.Up, k.Down, k.Back}
		}
		return []key.Binding{k.Back}
	default: // listView
		return []key.Binding{k.Up, k.Down, enterAs(k, "detail"), k.Shell, k.Refresh, k.Filter, k.Sort, k.Group, k.Help, k.Quit}
	}
}

// enterAs returns the Enter binding with a view-specific footer label. The
// underlying keys are unchanged so key.Matches against the canonical k.Enter
// still works; only the help text differs.
func enterAs(k keyMap, desc string) key.Binding {
	b := k.Enter
	b.SetHelp("enter", desc)
	return b
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
			{enterAs(k, "browse"), k.Shell, k.Browse},
			{k.Mark, k.Diff, k.Info},
			{k.Group, k.Collapse},
			{k.Refresh, k.Back},
		}
	case browseView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{enterAs(k, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell},
			{k.Back},
		}
	case findVersionsView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{k.HostToggle, k.Back},
		}
	case snapshotDiffView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{enterAs(k, "open"), k.Parent, k.Search, k.DiffSwap},
			{k.DiffFilterAdded, k.DiffFilterRemoved, k.DiffFilterModified, k.DiffFilterMetadata, k.DiffFilterTypeChanged, k.DiffFilterBitrot},
			{k.Back},
		}
	case extractView:
		return [][]key.Binding{
			{k.Up, k.Down, k.Enter, k.ExtractGo, k.Target},
			{k.Shell, k.Keep, k.Delete, k.Back},
		}
	case helpView:
		return [][]key.Binding{
			{k.Back},
		}
	case infoView:
		if h.infoScrollable {
			return [][]key.Binding{
				{k.Up, k.Down, k.PageUp, k.PageDown, k.Back},
			}
		}
		return [][]key.Binding{
			{k.Back},
		}
	default: // listView
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{enterAs(k, "detail"), k.Shell},
			{k.Refresh, k.RefreshAll},
			{k.Filter, k.Sort, k.Group},
			{k.Help, k.Quit},
		}
	}
}
