// Package tui implements resticscope's Bubble Tea terminal interface, including
// its screens, rendering, input handling, and command plumbing.
package tui

import (
	"context"
	"image/color"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// view selects which screen the Model renders.
type view int

const (
	listView view = iota
	detailView
	browseView
	findVersionsView
	snapshotDiffView
	helpView
	infoView
	extractView
	diffInfoView
)

// Model is the root Bubble Tea model coordinating all application views and
// their transient UI state.
type Model struct {
	app        *app.App
	ctx        context.Context    // scopes in-flight refreshes; cancelled on quit
	cancel     context.CancelFunc // cancels ctx so quitting kills any running restic
	keys       keyMap
	styles     styles
	help       help.Model
	spinner    spinner.Model
	rows       []app.RepoStatus
	meta       map[string]rowMeta
	view       view
	prevView   view            // view to restore when the help overlay closes
	cursor     int             // selected repo in the visible (filtered/sorted) list
	snapCursor int             // selected snapshot in the detail view
	detailName string          // repo the detail view is pinned to (set on enter)
	sortMode   sortMode        // order applied to the list view
	filter     string          // active filter query (name/region/label substring)
	filtering  bool            // true while the user is typing a filter
	groupIndex int             // transient group selector: 0 is flat; 1..N indexes cfg.Global.GroupBy
	pending    map[string]bool // repo name -> a refresh is in flight
	sem        chan struct{}   // bounds concurrent refreshes to parallelism
	resticVer  string
	width      int
	height     int
	statusMsg  string // transient footer notice (e.g. a cache-save warning)
	quitting   bool

	// termBg and termFg are painted on every View, reset on quit, and restored
	// after shell-outs. They are nil when theme background painting is disabled;
	// colorOK prevents painting in NO_COLOR and dumb-terminal sessions.
	termBg, termFg color.Color
	colorOK        bool

	// Browse rows are session-only and cleared on leaving browse. Filenames remain
	// in app.Browse's session-scoped encrypted store until exit so revisiting an
	// indexed snapshot is immediate; clearBrowse drops only this UI state.
	browseRows      []model.BrowseEntry // the current directory's children, or nil
	browseRepo      string              // repo being browsed (pins the action target)
	browseSnapshot  string              // snapshot id being browsed
	browseDir       string              // path of the directory currently listed
	browseCursor    int                 // selected entry within the current directory
	browseSortMode  browseSortMode      // display order for the current dir listing; resets on leaving browse
	browseIndexed   bool                // the snapshot's one-time index has committed
	browseIndexN    int                 // running node count shown while indexing
	browseRate      rateSampler         // recent indexed-entry rate, in entries/sec
	isBrowseLoading bool                // an index or directory load is in flight (navigation paused)
	browseNotice    string              // browse-local one-action hint rendered in the fixed summary line
	browseCancel    context.CancelFunc  // cancels just the in-flight browse (child of m.ctx)
	browseGen       int                 // generation token; stale browse msgs are discarded
	browseProgress  chan int            // coalesced index-progress ticks; re-armed by waitForIndexProgress

	// browseCache makes back navigation synchronous. Snapshot listings are
	// immutable; clearBrowse drops their filenames and startBrowse prevents one
	// snapshot's root listing from serving another.
	browseCache map[string][]model.BrowseEntry

	// Search state stays separate so escape restores the directory listing.
	// Enter suspends the result set behind the jumped-to listing, and clearBrowse
	// removes the full paths held in its rows.
	browseSearching        bool                // true while the search input is open
	browseSearchSuspended  bool                // a search result set is parked behind a jumped-to listing; esc restores it
	browseSearchQuery      string              // the live search query
	browseSearchShownQuery string              // query that produced browseSearchRows; gates accept against in-flight edits
	browseSearchRows       []model.BrowseEntry // ranked matches (capped), full paths
	browseSearchCursor     int                 // selected match within browseSearchRows
	browseSearchTotal      int                 // total matches before the result cap
	browseSearchErr        string              // path-free search error, shown while searching

	// Find-version rows hold filenames and snapshot IDs only until the view is
	// cleared. Request and result filters stay separate so an in-flight toggle
	// cannot relabel rows from the previous response.
	findRepo            string
	findOriginHost      string              // originating snapshot hostname from the live TUI row
	findPath            string              // the path being searched (held by the model only)
	findRequestAllHosts bool                // user toggle position, flipped by `a`
	findResultHost      string              // host the latest response actually filtered by; "" when result.AllHosts
	findResultAllHosts  bool                // mirrors the latest response's AllHosts so the renderer can label without inference
	findRows            []model.FileVersion // distinct (size, mtime) versions, newest-first
	findCursor          int                 // selected version row
	findErr             string              // path-free first line of the find/restic error
	findGen             int                 // generation token; stale find msgs are discarded
	findCancel          context.CancelFunc

	// infoScroll is reset when the modal opens and clamped on every render.
	infoScroll int

	// helpScroll moves only on overflow, resets when help opens, and is clamped
	// on every render.
	helpScroll int

	// Detail marks form a two-slot FIFO. They survive detail sub-screens and are
	// cleared only when goBack leaves detail for the repository list.
	detailMarks []model.Snapshot

	// Snapshot grouping and tree-ID collapse are transient per detail visit;
	// goBack resets both when leaving detail for the repository list.
	snapGroupMode    snapGroupMode
	snapCollapseTree bool

	// Snapshot-diff paths remain in memory only until clearSnapshotDiff runs on
	// leaving the view.
	diffRepo       string
	diffOlder      model.Snapshot    // first snapshot in the displayed diff direction
	diffNewer      model.Snapshot    // second snapshot in the displayed diff direction
	diffEntries    []model.DiffEntry // entries accumulated by the streamed onEntry callback
	diffTree       model.DiffTree    // virtual tree built once from diffEntries on terminal msg
	diffDir        string            // path of the directory currently listed (defaults to DiffRoot)
	diffRows       []model.DiffRow   // current dir's children, filtered + sorted (rebuilt on nav/filter)
	diffMarkerCols int               // widest Change marker in diffRows, measured when they are rebuilt
	diffCursor     int               // cursor within diffRows
	diffCache      map[string]int    // visited dir -> remembered cursor index (back-nav restore)
	diffSelectPath string            // row path to reselect after an async diff rerun

	// Diff search is a modal overlay over the loaded diffEntries. It searches
	// changed paths only, never repository contents. Result rows hold paths only
	// for the active diff view and are cleared by clearSnapshotDiff.
	diffSearching     bool
	diffSearchQuery   string
	diffSearchRows    []model.DiffRow
	diffSearchCursor  int
	diffSearchTotal   int
	diffSearchOrigin  string
	diffSearchOrigCur int
	diffSearchJumped  bool

	diffFilters   model.ModifierKind // bitset of enabled change types; all on by default
	diffMetadata  bool               // loaded diff includes metadata-only changes; kept across diffs
	diffStats     model.DiffStats    // top-level totals (a copy of diffTree.Aggregate[DiffRoot])
	diffErr       string             // sticky partial-diff warning rendered with the loaded tree
	diffParseErrs int                // tolerated malformed diff lines in the completed stream
	diffLoadCount int                // entries seen on the wire while loading (progress UX)
	diffGen       int                // generation token; stale diff msgs are discarded
	diffCancel    context.CancelFunc // cancels just the in-flight diff (child of m.ctx)
	diffProgress  chan int           // coalesced count-of-entries-seen ticks; re-armed by waitForDiffProgress

	// Diff info holds one changed path's records from each snapshot while its
	// screen is open. dropDiffInfo clears them and cancels a pending lookup.
	diffInfoRow    model.DiffRow      // the row the screen describes
	diffInfoFirst  diffInfoSide       // record from diffOlder
	diffInfoSecond diffInfoSide       // record from diffNewer
	diffInfoScroll int                // modal scroll offset, clamped on render
	diffInfoGen    int                // generation token; stale lookups are discarded
	diffInfoCancel context.CancelFunc // cancels the in-flight lookup; nil once it lands

	// The extract sub-model owns its state machine, cancellation, file picker,
	// and transient clearing. Model routes its input and restores the prior view
	// when extraction closes.
	extract extractModel

	// extractReturn selects the originating view; stale or unset values fall back
	// to browse.
	extractReturn view

	// extractTargetMemo holds the most recently used extraction root in memory
	// only; picker-only selections are forgotten. It is never written to state,
	// logs, or caches, and remember_target=false disables it.
	extractTargetMemo string
}

// Run loads cached state, enters the alternate screen, and blocks until quit.
// The caller must initialize App secrets and Restic first so passphrase prompts
// occur before entering the alternate screen.
func Run(ctx context.Context, a *app.App, resticVer string) error {
	return runProgram(ctx, a, resticVer)
}

func runProgram(ctx context.Context, a *app.App, resticVer string, opts ...tea.ProgramOption) error {
	// Keep operation cancellation separate so a normal quit stops restic work
	// without making Program.Run report context cancellation.
	opCtx, cancelOps := context.WithCancel(ctx)
	defer cancelOps()

	rows, err := a.Statuses(opCtx)
	if err != nil {
		return err
	}

	opts = append([]tea.ProgramOption{tea.WithContext(ctx)}, opts...)
	_, err = tea.NewProgram(newModel(opCtx, cancelOps, a, rows, resticVer), opts...).Run()
	return err
}

func newModel(ctx context.Context, cancel context.CancelFunc, a *app.App, rows []app.RepoStatus, resticVer string) Model {
	st := newStyles(a.Cfg.Theme.Palette())
	helpModel := help.New()
	helpModel.Styles = st.help

	m := Model{
		app:       a,
		ctx:       ctx,
		cancel:    cancel,
		keys:      defaultKeys(),
		styles:    st,
		help:      helpModel,
		spinner:   spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(st.spinner)),
		rows:      rows,
		meta:      buildMeta(a.Cfg),
		pending:   make(map[string]bool),
		sem:       make(chan struct{}, parallelism(a.Cfg)),
		resticVer: resticVer,
	}
	m.termBg, m.termFg = themeTerminalColors(a.Cfg.Theme)
	// Start with the first configured group; `g` cycles the rest and flat mode.
	// Group selection is never persisted.
	if len(a.Cfg.Global.GroupBy) > 0 {
		m.groupIndex = 1
	}
	if a.Cfg.Global.RefreshOnOpen {
		for _, name := range m.refreshOnOpenNames() {
			m.pending[name] = true
		}
	}
	return m
}

func parallelism(cfg *config.Config) int {
	if n := cfg.Global.Parallelism; n > 0 {
		return n
	}
	return 1
}

// refreshOnOpenNames lists repos that should be refreshed when the TUI opens:
// those never refreshed or whose cache is older than stale_after.
func (m Model) refreshOnOpenNames() []string {
	var names []string
	for _, r := range m.rows {
		if !r.State.Refreshed() || r.Stale {
			names = append(names, r.Name)
		}
	}
	return names
}

// Init implements tea.Model: it seeds refresh-on-open commands for stale repos
// and starts the spinner when any refresh is pending.
func (m Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.app.Cfg.Global.RefreshOnOpen {
		for _, name := range m.refreshOnOpenNames() {
			cmds = append(cmds, m.refreshCmd(name))
		}
	}
	// Start the spinner only when refresh_on_open seeded pending repos, to avoid
	// re-rendering a hidden idle spinner.
	if len(m.pending) > 0 {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model: the central message dispatcher, a flat
// type-switch over message kinds whose cases share Model state.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,funlen // central bubbletea message dispatcher; a flat type-switch whose cases share Model state, so splitting would reduce, not improve, readability
	switch msg := msg.(type) {
	case tea.ColorProfileMsg:
		// Paint the theme only for ANSI-capable profiles; NO_COLOR and dumb
		// terminals retain their defaults.
		m.colorOK = msg.Profile >= colorprofile.ANSI
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.SetWidth(msg.Width)
		m.extract.setHeight(msg.Height)
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case repoRefreshedMsg:
		return m.applyRefresh(msg), nil
	case browseIndexProgressMsg:
		return m.applyBrowseIndexProgress(msg)
	case browseIndexedMsg:
		return m.applyBrowseIndexed(msg)
	case browseDirMsg:
		return m.applyBrowseDir(msg), nil
	case browseSearchMsg:
		return m.applyBrowseSearch(msg), nil
	case findVersionsMsg:
		return m.applyFindVersionsMsg(msg), nil
	case snapshotDiffProgressMsg:
		return m.applySnapshotDiffProgressMsg(msg)
	case snapshotDiffMsg:
		return m.applySnapshotDiffMsg(msg), nil
	case diffInfoMsg:
		return m.applyDiffInfoMsg(msg), nil
	case shellExitedMsg:
		return m.applyShellExit(msg), nil
	case extractRunDoneMsg:
		// Reject late messages from a prior extract session, whose generation may
		// have restarted at zero. The command advances a queued diff extraction.
		if m.extractActive() {
			return m, m.extract.applyRunDone(msg)
		}
		return m, nil
	case extractCountsMsg:
		if m.extractActive() {
			m.extract.applyCounts(msg)
		}
		return m, nil
	case extractProgressMsg:
		if !m.extractActive() {
			return m, nil
		}
		cmd := m.extract.applyProgress(msg)
		return m, cmd
	case extractSudoProbeMsg:
		if !m.extractActive() {
			return m, nil
		}
		cmd := m.extract.applySudoProbe(msg)
		return m, cmd
	case extractSudoAuthMsg:
		if !m.extractActive() {
			return m, nil
		}
		cmd := m.extract.applySudoAuth(msg)
		return m, cmd
	case extractDeleteStagingDoneMsg:
		if !m.extractActive() {
			return m, nil
		}
		// Route closing through the single extractBackToBrowseMsg path below.
		cmd := m.extract.applyDeleteStagingDone(msg)
		return m, cmd
	case extractBackToBrowseMsg:
		if !m.extractActive() {
			return m, nil
		}
		// Cancel in-flight work and zero all transient path fields. Only the target
		// of an extraction that actually ran may survive, and only when configured.
		if t := m.extract.ranTargetRoot; t != "" && m.app.Cfg.Extract.RememberTarget {
			m.extractTargetMemo = t
		}
		m.extract.supersede()
		m.extract = extractModel{}
		switch m.extractReturn {
		case detailView, findVersionsView, snapshotDiffView:
			m.view = m.extractReturn
		default:
			m.view = browseView
		}
		m.extractReturn = listView
		if msg.notice != "" {
			m.statusMsg = msg.notice
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Stop the tick loop once pending drains because the spinner is hidden at
		// idle; refresh keys restart it on the next idle-to-refreshing edge.
		if len(m.pending) == 0 {
			return m, nil
		}
		return m, cmd
	default:
		// The embedded filepicker is fully async: its Init and navigation emit an
		// unexported readDirMsg no case above handles. Forward unhandled messages
		// to it while the overlay is open so the directory list populates.
		if m.extractActive() && m.extract.state == extractStateFilePicker {
			var cmd tea.Cmd
			m.extract, cmd = m.extract.updateFilePicker(msg)
			return m, cmd
		}
	}
	return m, nil
}

// extractActive reports whether extraction is visible or behind its help
// overlay. Async extract and file-picker messages remain active behind help so
// they cannot strand the modal.
func (m Model) extractActive() bool {
	return m.view == extractView || (m.view == helpView && m.prevView == extractView)
}
