package tui

import "charm.land/bubbles/v2/key"

// keyMap contains bindings shared across views. Per-view helpers adapt it to
// help.KeyMap so the footer shows only relevant actions.
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	SearchUp   key.Binding // Search results: up or ctrl+k, never literal k.
	SearchDown key.Binding // Search results: down or ctrl+j, never literal j.
	PageUp     key.Binding
	PageDown   key.Binding
	Enter      key.Binding
	Back       key.Binding
	Shell      key.Binding
	Browse     key.Binding
	Parent     key.Binding // Browse: open the parent directory.
	Open       key.Binding // Browse: open the selected directory; aliases Enter.
	Refresh    key.Binding
	RefreshAll key.Binding
	Filter     key.Binding
	Search     key.Binding // Browse: open filename search; shares / with Filter.
	Sort       key.Binding
	Group      key.Binding // List/detail: cycle the available grouping modes.
	Collapse   key.Binding // Detail: collapse consecutive snapshots with the same tree ID.
	Versions   key.Binding // Browse: find versions of the selected file.
	HostToggle key.Binding // Find versions: toggle the host filter.
	Mark       key.Binding // Detail: toggle the snapshot in the two-slot diff FIFO.
	Diff       key.Binding // Detail: open the resolved older-to-newer diff.
	Info       key.Binding // Detail: open snapshot information.
	DiffSwap   key.Binding // Diff: reverse the snapshot pair and rerun.
	DiffMeta   key.Binding // Diff: rerun with or without metadata-only changes.
	Extract    key.Binding // Extract the selection appropriate to the current view.
	Target     key.Binding // Extract: choose the target root.
	Priv       key.Binding // Extract: toggle privileged restore.
	Keep       key.Binding // Extract: keep staging after cancellation or failure.
	Delete     key.Binding // Extract: delete staging after cancellation or failure.
	Help       key.Binding
	Quit       key.Binding // Context-aware q: back when nested, quit on the list.
	HardQuit   key.Binding // Unconditional ctrl+c.

	// Each diff filter toggles one model.ModifierKind bit. Bitrot uses b because
	// its native ? character collides with Help.
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
		// Keep j/k available for queries; arrows and ctrl+j/ctrl+k navigate results.
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
		DiffMeta:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "metadata")),
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

// viewHelp adapts keyMap to help.KeyMap for the active view. It relabels shared
// keys and substitutes bindings for active filter and search inputs.
//
// ShortHelp preserves one relative action order across views.
type viewHelp struct {
	keys            keyMap
	view            view
	filtering       bool
	searching       bool // Browse filename-search input is open.
	infoScrollable  bool // Info body overflows; show scroll bindings.
	helpScrollable  bool // Help body overflows; show scroll bindings.
	searchSuspended bool // Browse results are parked; esc restores them.
	diffJumped      bool // A diff search jump is armed; esc or q reverses it.

	// extractBindings come from extractModel.shortHelp, which owns the modal's
	// state machine.
	extractBindings []key.Binding
}

func (h viewHelp) ShortHelp() []key.Binding {
	k := h.keys
	if h.filtering {
		return []key.Binding{k.FilterAccept, k.FilterCancel}
	}
	// Search input replaces the regular browse bindings while it is open.
	if h.searching {
		return []key.Binding{searchMoveHelp(), k.SearchAccept, k.SearchCancel}
	}
	switch h.view {
	case detailView:
		return []key.Binding{moveHelp(), helpAs(k.Enter, "browse"), k.Mark, k.Diff, k.Info, k.Extract, k.Group, k.Shell, k.Back}
	case browseView:
		if h.searchSuspended {
			// esc restores parked results, while q leaves browse.
			return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell, escHelp("results"), k.Back}
		}
		return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Parent, k.Search, k.Versions, k.Extract, k.Sort, k.Shell, k.Back}
	case findVersionsView:
		return []key.Binding{moveHelp(), helpAs(k.Enter, "extract"), k.HostToggle, k.Back}
	case snapshotDiffView:
		// Parent stays in the help overlay so the bar fits its width budget.
		if h.diffJumped {
			// After a search jump, q and esc reverse the jump instead of leaving.
			return []key.Binding{
				moveHelp(), helpAs(k.Enter, "open"), k.Search, k.Extract, k.DiffSwap, k.DiffMeta, diffFiltersHelp(),
				key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc/q", "previous")),
			}
		}
		return []key.Binding{moveHelp(), helpAs(k.Enter, "open"), k.Search, k.Extract, k.DiffSwap, k.DiffMeta, diffFiltersHelp(), k.Back}
	case extractView:
		// Preserve a back affordance if the extract model supplied no bindings.
		if len(h.extractBindings) > 0 {
			return h.extractBindings
		}
		return []key.Binding{k.Back}
	case helpView:
		// Show "scroll" only when the document viewport overflows.
		if h.helpScrollable {
			return []key.Binding{helpAs(moveHelp(), "scroll"), k.Back}
		}
		return []key.Binding{k.Back}
	case infoView:
		// Show the canonical back key and, only on overflow, document scrolling.
		if h.infoScrollable {
			return []key.Binding{helpAs(moveHelp(), "scroll"), k.Back}
		}
		return []key.Binding{k.Back}
	default: // listView
		// The title already provides the persistent help affordance.
		return []key.Binding{moveHelp(), helpAs(k.Enter, "detail"), k.Filter, k.Sort, k.Group, k.Shell, k.Refresh, k.Quit}
	}
}

// helpAs changes only a binding's view-specific footer description, preserving
// its keys for key.Matches.
func helpAs(b key.Binding, desc string) key.Binding {
	b.SetHelp(b.Help().Key, desc)
	return b
}

// escHelp creates a footer-only chip where esc differs from q. Back cannot be
// relabeled because its displayed key is q.
func escHelp(desc string) key.Binding {
	return key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", desc))
}

// moveHelp combines cursor bindings into one compact, consistent footer chip.
// The full help overlay still lists j/k.
func moveHelp() key.Binding {
	return key.NewBinding(key.WithKeys("up", "down", "j", "k"), key.WithHelp("↑/↓", "move"))
}

// searchMoveHelp keeps j/k as query text while arrows and ctrl+j/k navigate.
// The footer shows arrows; the full help overlay lists the alternatives.
func searchMoveHelp() key.Binding {
	return key.NewBinding(key.WithKeys("up", "ctrl+k", "down", "ctrl+j"), key.WithHelp("↑/↓", "move"))
}

// diffFiltersHelp combines six independent toggles into the compact mask shown
// in the diff summary. Avoiding slash separators distinguishes independent
// actions from alternate keys and keeps q visible at 80 columns.
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
			{helpAs(k.Enter, "open"), k.Parent, k.Search, k.Extract, k.DiffSwap, k.DiffMeta},
			{k.DiffFilterAdded, k.DiffFilterRemoved, k.DiffFilterModified, k.DiffFilterMetadata, k.DiffFilterTypeChanged, k.DiffFilterBitrot},
			{k.Back},
		}
	case extractView:
		return [][]key.Binding{
			{k.Enter, k.Target, k.Priv},
			{k.Shell, k.Keep, k.Delete, k.Back},
		}
	case helpView:
		if h.helpScrollable {
			return [][]key.Binding{
				{k.Up, k.Down, k.PageUp, k.PageDown, k.Back},
			}
		}
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
