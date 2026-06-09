package tui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
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
)

// Model is the root Bubble Tea model. It drives both the list view and the
// per-repo detail view; view selects which one is showing.
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
	groupIndex int             // transient: 0 = flat view; 1..N picks cfg.Global.GroupBy[i-1] as the active group key
	pending    map[string]bool // repo name -> a refresh is in flight
	sem        chan struct{}   // bounds concurrent refreshes to parallelism
	resticVer  string
	width      int
	height     int
	statusMsg  string // transient footer notice (e.g. a cache-save warning)
	quitting   bool

	// Browse state, kept off disk by design. The on-screen rows are session-only — they are never persisted
	// to the cache, RepoState, or any log, and leaving browse clears them. The
	// underlying filenames live only in the session-scoped encrypted store
	// (app.Browse), which survives until the app exits so returning to an
	// already-indexed snapshot is instant; clearBrowse drops only the UI state.
	browseRows       []model.BrowseEntry // the current directory's children, or nil
	browseRepo       string              // repo being browsed (pins the action target)
	browseSnapshot   string              // snapshot id being browsed
	browseDir        string              // path of the directory currently listed
	browseCursor     int                 // selected entry within the current directory
	browseSortMode   browseSortMode      // display order for the current dir listing; resets on leaving browse
	browseIndexed    bool                // the snapshot's one-time index has committed
	browseIndexN     int                 // running node count shown while indexing
	browseIndexRate  float64             // recent indexed-entry rate, in entries/sec
	browseRateBaseN  int                 // count at the start of the current rate window
	browseRateBaseAt time.Time           // timestamp at the start of the current rate window
	browseLoading    bool                // an index or directory load is in flight (navigation paused)
	browseNotice     string              // browse-local one-action hint rendered in the fixed summary line
	browseCancel     context.CancelFunc  // cancels just the in-flight browse (child of m.ctx)
	browseGen        int                 // generation token; stale browse msgs are discarded
	browseProgress   chan int            // coalesced index-progress ticks; re-armed by waitForIndexProgress

	// browseCache memoizes visited directories' listings for the current browse so
	// back/parent navigation is served synchronously (no async query, no loading
	// hop). A committed snapshot is immutable, so an entry never goes stale — no
	// invalidation. It holds filenames, so clearBrowse drops it on leaving browse
	// (non-negotiable #1: no filenames linger), and startBrowse resets it so one
	// snapshot's "/" can never serve another's.
	browseCache map[string][]model.BrowseEntry

	// Global filename search state, kept entirely SEPARATE from the directory
	// listing above to ensure cancelling search (esc) restores the prior listing untouched.
	// Enter does not exit the search: it SUSPENDS it (browseSearchSuspended), keeping
	// the query/rows/cursor so esc from the jumped-to listing can restore them. The
	// rows hold full paths/filenames for the lifetime of the model only; clearBrowse
	// zeros every field here on leaving browse (non-negotiable #1: no filenames linger
	// once the user leaves browse).
	browseSearching        bool                // true while the search input is open
	browseSearchSuspended  bool                // a search result set is parked behind a jumped-to listing; esc restores it
	browseSearchQuery      string              // the live search query
	browseSearchShownQuery string              // query that produced browseSearchRows; gates accept against in-flight edits
	browseSearchRows       []model.BrowseEntry // ranked matches (capped), full paths
	browseSearchCursor     int                 // selected match within browseSearchRows
	browseSearchTotal      int                 // total matches before the result cap
	browseSearchErr        string              // path-free search error, shown while searching

	// Find-versions state. Like browse, rows hold a filename and snapshot ids
	// only for the lifetime of the model — clearFindVersions zeroes them on
	// leaving the view. The split between findRequestAllHosts (user toggle) and
	// findResultAllHosts/findResultHost (filter the visible rows came from) is
	// deliberate: the renderer reads only the result fields, so a mid-toggle
	// reload can never relabel rows that came from the other filter.
	findRepo            string
	findOriginHost      string              // originating snapshot hostname from the live TUI row
	findPath            string              // the path being searched (held by the model only)
	findRequestAllHosts bool                // user toggle position, flipped by `a`
	findLoading         bool                // a find call is in flight
	findResultHost      string              // host the latest response actually filtered by; "" when result.AllHosts
	findResultAllHosts  bool                // mirrors the latest response's AllHosts so the renderer can label without inference
	findRows            []model.FileVersion // distinct (size, mtime) versions, newest-first
	findCursor          int                 // selected version row
	findErr             string              // path-free first line of the find/restic error
	findGen             int                 // generation token; stale find msgs are discarded
	findCancel          context.CancelFunc

	// Vertical scroll offset for the snapshot-info modal body, in lines. Reset
	// to 0 when the modal opens; clamped to a valid range on every render.
	infoScroll int

	// Detail-view 2-slot FIFO of marked snapshots. Marks belong to the "detail
	// context": they survive a round-trip into the snapshot-diff or browse views
	// (those are sub-screens reached from detail) and clear only when leaving the
	// detail context for the list. The clear lives in goBack's detailView arm so
	// every back path goes through the same gate.
	detailMarks []model.Snapshot

	// Snapshot-table grouping and tree-ID collapse for the detail view. Both are
	// transient per-detail-visit state: snapGroupMode cycles via `g` (off → host
	// → tags → paths → off) and snapCollapseTree toggles via `c`. A fresh detail
	// visit starts with both off; goBack on the detail arm resets both alongside
	// clearDetailMarks so a return to detail starts fresh.
	snapGroupMode    snapGroupMode
	snapCollapseTree bool

	// Snapshot-diff view state. Same path-no-persist discipline as findRows and
	// browseRows: clearSnapshotDiff zeros every diff* field on leaving the view.
	diffRepo       string
	diffOlder      model.Snapshot    // first snapshot in the displayed diff direction
	diffNewer      model.Snapshot    // second snapshot in the displayed diff direction
	diffEntries    []model.DiffEntry // entries accumulated by the streamed onEntry callback
	diffTree       model.DiffTree    // virtual tree built once from diffEntries on terminal msg
	diffDir        string            // path of the directory currently listed (defaults to DiffRoot)
	diffRows       []model.DiffRow   // current dir's children, filtered + sorted (rebuilt on nav/filter)
	diffCursor     int               // cursor within diffRows
	diffCache      map[string]int    // visited dir → remembered cursor index (back-nav restore)
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
	diffStats     model.DiffStats    // top-level totals (a copy of diffTree.Aggregate[DiffRoot])
	diffErr       string             // sticky partial-diff warning rendered with the loaded tree
	diffParseErrs int                // tolerated malformed diff lines in the completed stream
	diffLoading   bool               // a diff stream is in flight (navigation paused)
	diffLoadCount int                // entries seen on the wire while loading (progress UX)
	diffGen       int                // generation token; stale diff msgs are discarded
	diffCancel    context.CancelFunc // cancels just the in-flight diff (child of m.ctx)
	diffProgress  chan int           // coalesced count-of-entries-seen ticks; re-armed by waitForDiffProgress

	// Extract sub-model. Constructed on `e` from browse (step 07) and hosted
	// here while m.view == extractView. The sub-model owns its own state
	// machine, generation token, per-op cancel, embedded filepicker, and
	// transient-clear discipline; the root Model just routes keys and messages
	// to it and swaps view back to browse on extractBackToBrowseMsg.
	extract extractModel
}

// Run loads cached state for an instant first paint, then starts the program in
// the alternate screen and blocks until the user quits. The caller must already
// have wired the App's Secrets and Restic (i.e. run secrets_command) so any GPG
// passphrase prompt happens before the alt-screen is entered (plan §12).
func Run(ctx context.Context, a *app.App, resticVer string) error {
	return runProgram(ctx, a, resticVer)
}

func runProgram(ctx context.Context, a *app.App, resticVer string, opts ...tea.ProgramOption) error {
	// A cancellable child scopes refresh/browse goroutines. It is intentionally
	// separate from Bubble Tea's program context: normal q/ctrl+c quits must cancel
	// restic work without making Program.Run report "context canceled".
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
	st := newStyles()
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
	// Start grouped by the first configured key when any key is configured; the
	// user cycles through the rest (and back to flat) with `g`. Transient, never
	// persisted.
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

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.SetWidth(msg.Width)
		// Keep the extract sub-model sized too: it owns an embedded filepicker
		// whose viewport we size ourselves (AutoHeight is off), so a live resize
		// while it is open must reflow.
		m.extract.height = msg.Height
		if m.extract.filepickerInit {
			m.extract.filepicker.SetHeight(extractFilePickerHeight(msg.Height))
		}
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
	case shellExitedMsg:
		return m.applyShellExit(msg), nil
	case extractRunDoneMsg:
		// Every extract message is gated on the modal being active: the sub-model's
		// gen restarts at 0 each session, so a late message from a prior session
		// must not be applied (or worse, switch the view) once the user has left.
		// "Active" includes the help overlay opened over extract — otherwise a
		// completion that lands while help is open would be dropped and strand the
		// modal (it returns to extract via prevView, not a real exit).
		if m.extractActive() {
			m.extract.applyRunDone(msg)
		}
		return m, nil
	case extractProgressMsg:
		if !m.extractActive() {
			return m, nil
		}
		cmd := m.extract.applyProgress(msg)
		return m, cmd
	case extractDeleteStagingDoneMsg:
		if !m.extractActive() {
			return m, nil
		}
		m.extract.applyDeleteStagingDone(msg)
		// Closing the modal lives here so the sub-model stays self-contained.
		notice := m.extract.noticeAfterClose
		m.extract.clearTransient()
		m.extract = extractModel{}
		m.view = browseView
		if notice != "" {
			m.statusMsg = notice
		}
		return m, nil
	case extractBackToBrowseMsg:
		if !m.extractActive() {
			return m, nil
		}
		m.extract.clearTransient()
		m.extract = extractModel{}
		m.view = browseView
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
		// The embedded filepicker is fully async: its Init and every navigation
		// emit an unexported readDirMsg that no case above handles. Forward any
		// otherwise-unhandled message to it while the overlay is open so the
		// directory list actually populates (without this it renders empty).
		if m.extractActive() && m.extract.state == extractStateFilePicker {
			var cmd tea.Cmd
			m.extract, cmd = m.extract.updateFilePicker(msg)
			return m, cmd
		}
	}
	return m, nil
}

// extractActive reports whether the extract sub-model is the live modal context:
// either showing directly, or temporarily behind the help overlay (which was
// opened over it and returns to it via prevView). Extract async messages and the
// filepicker's async reads must be honored in both — help is an overlay, not a
// real extract exit, and clearTransient is not called when it opens — otherwise a
// run completion, progress tick, or staging delete that lands while help is up
// would be silently dropped and strand the modal.
func (m Model) extractActive() bool {
	return m.view == extractView || (m.view == helpView && m.prevView == extractView)
}
