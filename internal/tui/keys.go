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
	Extract    key.Binding // browse: extract the selected entry; detail: the whole snapshot; find-versions: the selected version
	Target     key.Binding // extract: open the target-root filepicker overlay from review
	Priv       key.Binding // extract: toggle the privileged (sudo) restore on review
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
		Parent:     key.NewBinding(key.WithKeys("backspace", "left", "h"), key.WithHelp("⌫", "parent")),
		Open:       key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→", "open")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		RefreshAll: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all")),
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Search:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Sort:       key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "sort")),
		Group:      key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "group")),
		Collapse:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "collapse")),
		Versions:   key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "versions")),
		HostToggle: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "all hosts")),
		Mark:       key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "mark")),
		Diff:       key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "diff")),
		Info:       key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info")),
		DiffSwap:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "swap")),
		Extract:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "extract")),
		Target:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "target")),
		Priv:       key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "as root")),
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
// navigation, filter/sort/group, shell/refresh, and quit; the detail view swaps
// in the snapshot-scoped keys and back (advertised as q, with esc still bound),
// where q steps back rather than quits; the help overlay shows only back (the
// overlay itself is the full reference). The list keeps Quit because q only exits
// there. While the user is typing a filter (filtering), it shows the apply/clear
// bindings instead. Enter does something different in each view (open detail in
// the list, browse the selected snapshot in detail, open directory in browse),
// so its footer label is overridden per view via helpAs below.
//
// Every ShortHelp keeps one chip order so reused keys sit in the same relative
// place across views: move → enter → ⌫ → / → view actions → sort/group →
// shell/refresh → back/quit.
type viewHelp struct {
	keys            keyMap
	view            view
	filtering       bool
	searching       bool // browse global filename search input is open
	infoScrollable  bool // info modal body overflows; advertise up/down in the footer
	searchSuspended bool // browse: a search result set is parked; esc restores it
	diffJumped      bool // diff: a search jump is armed; esc (and q) reverse it

	// extractBindings are the extract modal's per-state footer bindings,
	// supplied by extractModel.shortHelp (the sub-model owns its state machine,
	// so footerView fills this in when the extract view is active).
	extractBindings []key.Binding
}

func (h viewHelp) ShortHelp() []key.Binding {
	k := h.keys
	if h.filtering {
		return []key.Binding{k.FilterAccept, k.FilterCancel}
	}
	// While the global filename search is open the cursor keys move through the
	// matches and enter/esc open/cancel; the regular browse keys are suspended.
	if h.searching {
		return []key.Binding{searchMoveHelp(), k.SearchAccept, k.SearchCancel}
	}
	switch h.view {
	case detailView:
		return []key.Binding{moveHelp(), helpAs(k.Enter, "browse"), k.Mark, k.Diff, k.Info, k.Extract, k.Group, k.Shell, k.Back}
	case browseView:
		if h.searchSuspended {
			// esc restores the parked search while q leaves browse outright
			// (browse.go's deliberate asymmetry), so both chips are correct
			// side by side.
			return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell, escHelp("results"), k.Back}
		}
		return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell, k.Back}
	case findVersionsView:
		return []key.Binding{moveHelp(), helpAs(k.Enter, "extract"), k.HostToggle, k.Back}
	case snapshotDiffView:
		if h.diffJumped {
			// After a search jump q mirrors esc and reverses the jump instead
			// of leaving the view (routing.go), so the normal `q back` chip
			// would lie; one combined chip replaces it until the jump is undone.
			return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Extract, k.DiffSwap, diffFiltersHelp(),
				key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc/q", "previous"))}
		}
		return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Extract, k.DiffSwap, diffFiltersHelp(), k.Back}
	case extractView:
		// Per-state bindings from the extract sub-model; fall back to the
		// always-present back affordance if a caller forgot to supply them.
		if len(h.extractBindings) > 0 {
			return h.extractBindings
		}
		return []key.Binding{k.Back}
	case helpView:
		return []key.Binding{k.Back}
	case infoView:
		// The footer carries only the canonical back key (i and esc also close
		// the modal) — plus a scroll chip when the body overflows. "scroll",
		// not "move": the keys move a document viewport, not a cursor.
		if h.infoScrollable {
			return []key.Binding{helpAs(moveHelp(), "scroll"), k.Back}
		}
		return []key.Binding{k.Back}
	default: // listView
		// No ? help chip here: the title row carries the persistent help
		// affordance in every view, so the footer advertising it too would be
		// the one view that duplicates it.
		return []key.Binding{moveHelp(), helpAs(k.Enter, "detail"), k.Filter, k.Sort, k.Group, k.Shell, k.Refresh, k.Quit}
	}
}

// helpAs returns b with a view-specific footer description. The underlying keys
// are unchanged so key.Matches against the canonical binding still works; only
// the help text differs (e.g. Enter advertises "open"/"browse"/"extract" per
// view, Back advertises "cancel" while an extract runs).
func helpAs(b key.Binding, desc string) key.Binding {
	b.SetHelp(b.Help().Key, desc)
	return b
}

// escHelp is a footer-only chip for the states where esc does something q does
// not (restore a parked browse search). helpAs(k.Back, …) would be wrong here:
// Back's displayed key is deliberately "q", so the chip would read "q results".
func escHelp(desc string) key.Binding {
	return key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", desc))
}

// moveHelp is the condensed "↑/↓ move" footer entry every view's key bar uses:
// the two cursor-movement bindings collapse into one chip so the bar stays
// short on an ~100-col terminal and reads identically everywhere. j/k still
// move (the ? overlay lists them); only the footer hint is condensed.
func moveHelp() key.Binding {
	return key.NewBinding(key.WithKeys("up", "down", "j", "k"), key.WithHelp("↑/↓", "move"))
}

// searchMoveHelp is moveHelp's twin for the search-input footers, where plain
// j/k are literal query text and the real bindings are the arrows plus
// ctrl+j/k (SearchUp/SearchDown). The chip shows only the arrow form; the ?
// overlay documents the ctrl variants.
func searchMoveHelp() key.Binding {
	return key.NewBinding(key.WithKeys("up", "ctrl+k", "down", "ctrl+j"), key.WithHelp("↑/↓", "move"))
}

// diffFiltersHelp condenses the six diff filter-toggle bindings into one
// footer chip — advertising a single toggle (the old `+ added`) implied the
// others didn't exist, and listing all six as chips would flood the bar. The
// key display is the compact run the summary's `filter: +-MUT?` mask already
// uses (not slash-joined: slashes mean alternates of one action, these are six
// toggles), which also keeps the normal diff bar inside 80 columns with
// `q back` visible. The per-key meanings live in the ? overlay; the active
// mask is state and renders in the summary line, not here.
func diffFiltersHelp() key.Binding {
	return key.NewBinding(key.WithKeys("+", "-", "M", "U", "T", "b"), key.WithHelp("+-MUTb", "filters"))
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
			{helpAs(k.Enter, "browse"), k.Shell, k.Browse, k.Extract},
			{k.Mark, k.Diff, k.Info},
			{k.Group, k.Collapse},
			{k.Refresh, k.Back},
		}
	case browseView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{helpAs(k.Enter, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell},
			{k.Back},
		}
	case findVersionsView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{helpAs(k.Enter, "extract"), k.HostToggle, k.Extract, k.Back},
		}
	case snapshotDiffView:
		return [][]key.Binding{
			{k.Up, k.Down, k.PageUp, k.PageDown},
			{helpAs(k.Enter, "open"), k.Parent, k.Search, k.Extract, k.DiffSwap},
			{k.DiffFilterAdded, k.DiffFilterRemoved, k.DiffFilterModified, k.DiffFilterMetadata, k.DiffFilterTypeChanged, k.DiffFilterBitrot},
			{k.Back},
		}
	case extractView:
		return [][]key.Binding{
			{k.Enter, k.Target, k.Priv},
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
			{helpAs(k.Enter, "detail"), k.Filter, k.Sort, k.Group},
			{k.Shell, k.Refresh, k.RefreshAll},
			{k.Help, k.Quit},
		}
	}
}
