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
	helpView
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
	findSnapshot        string              // originating snapshot id
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
	case shellExitedMsg:
		return m.applyShellExit(msg), nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Stop the tick loop once pending drains because the spinner is hidden at
		// idle; refresh keys restart it on the next idle-to-refreshing edge.
		if len(m.pending) == 0 {
			return m, nil
		}
		return m, cmd
	}
	return m, nil
}
