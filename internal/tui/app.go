// Package tui is resticscope's Bubble Tea front end. It is a thin
// renderer/controller over the headless internal/app core: it holds no domain
// logic of its own, only view state (cursor, which repos are refreshing) and
// the commands that drive app.RefreshRow concurrently. Per the engineering
// rules it is tested via state transitions, not pixels.
package tui

import (
	"context"
	"os/exec"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
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
	coverageView
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
	cursor     int             // selected repo in the visible (filtered/sorted) list
	snapCursor int             // selected snapshot in the detail view
	detailName string          // repo the detail view is pinned to (set on enter)
	sortMode   sortMode        // order applied to the list view
	filter     string          // active filter query (name/region/label substring)
	filtering  bool            // true while the user is typing a filter
	pending    map[string]bool // repo name -> a refresh is in flight
	sem        chan struct{}   // bounds concurrent refreshes to parallelism
	resticVer  string
	width      int
	statusMsg  string // transient footer notice (e.g. a cache-save warning)
	quitting   bool
}

// Run loads cached state for an instant first paint, then starts the program in
// the alternate screen and blocks until the user quits. The caller must already
// have wired the App's Secrets and Restic (i.e. run secrets_command) so any GPG
// passphrase prompt happens before the alt-screen is entered (plan §12).
func Run(ctx context.Context, a *app.App, resticVer string) error {
	// A cancellable child scopes the refresh goroutines: defer cancel guarantees
	// they're torn down on every exit path, and the Model holds cancel so
	// quitting kills any in-flight restic immediately rather than orphaning it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rows, err := a.Statuses(ctx)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(newModel(ctx, cancel, a, rows, resticVer), tea.WithContext(ctx)).Run()
	return err
}

func newModel(ctx context.Context, cancel context.CancelFunc, a *app.App, rows []app.RepoStatus, resticVer string) Model {
	m := Model{
		app:       a,
		ctx:       ctx,
		cancel:    cancel,
		keys:      defaultKeys(),
		styles:    newStyles(),
		help:      help.New(),
		spinner:   spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		rows:      rows,
		meta:      buildMeta(a.Cfg),
		pending:   make(map[string]bool),
		sem:       make(chan struct{}, parallelism(a.Cfg)),
		resticVer: resticVer,
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
	cmds := []tea.Cmd{m.spinner.Tick}
	if m.app.Cfg.Global.RefreshOnOpen {
		for _, name := range m.refreshOnOpenNames() {
			cmds = append(cmds, m.refreshCmd(name))
		}
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.help.SetWidth(msg.Width)
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case repoRefreshedMsg:
		return m.applyRefresh(msg), nil
	case shellExitedMsg:
		return m.applyShellExit(msg), nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleKey dispatches a keypress. The keys that mean the same thing everywhere
// (quit, help, shell, refresh) are handled first; anything else is routed to the
// active view's handler, where ↑/↓ and enter carry view-specific meaning.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// While typing a filter, every key feeds the query (so "q", "r", etc. are
	// literal text); only apply/clear and ctrl+c escape it.
	if m.filtering {
		return m.handleFilterKey(msg)
	}

	// Keys that mean the same thing in every view: quit, help, and refresh-all
	// (which acts on all repos, so it needs no per-view cursor).
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		return m, nil
	case key.Matches(msg, m.keys.RefreshAll):
		m.statusMsg = ""
		var cmds []tea.Cmd
		for _, r := range m.rows {
			if cmd := m.startRefresh(r.Name); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	}

	// The coverage view has no per-repo cursor, so the repo-scoped keys below
	// (shell, refresh-current) do not apply there.
	if m.view == coverageView {
		return m.handleCoverageKey(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Shell):
		// `s` shells into the active repo with no snapshot context.
		if cmd := m.openShellCmd(nil); cmd != nil {
			m.statusMsg = ""
			return m, cmd
		}
		return m, nil
	case key.Matches(msg, m.keys.Refresh):
		if name, ok := m.actionRepo(); ok {
			m.statusMsg = ""
			return m, m.startRefresh(name)
		}
		return m, nil
	}

	if m.view == detailView {
		return m.handleDetailKey(msg)
	}
	return m.handleListKey(msg)
}

// handleCoverageKey handles input while the cross-repo coverage view is showing.
// Only navigation back to the list is meaningful; quit, help, and refresh-all
// are handled globally before this is reached.
func (m Model) handleCoverageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Back) {
		m.view = listView
	}
	return m, nil
}

func (m Model) handleListKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.visibleRows())-1 {
			m.cursor++
		}
	case key.Matches(msg, m.keys.Enter):
		// Pin the detail view to the selected repo by name so a later refresh
		// (which can reorder a size/staleness sort) can't swap it out.
		if row, ok := m.currentRow(); ok {
			m.detailName = row.Name
			m.view = detailView
			m.snapCursor = 0
		}
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
	case key.Matches(msg, m.keys.Sort):
		m = m.cycleSort()
	case key.Matches(msg, m.keys.Coverage):
		m.view = coverageView
	}
	return m, nil
}

// handleFilterKey consumes keys while the filter input is open. Apply keeps the
// query and returns to normal navigation; clear (esc) drops the query entirely;
// ctrl+c still quits. Every other key edits the query text. The cursor resets to
// the top whenever the query changes so it never points past the matches.
func (m Model) handleFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.String() == "ctrl+c":
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case key.Matches(msg, m.keys.FilterAccept):
		m.filtering = false
	case key.Matches(msg, m.keys.FilterCancel):
		m.filtering = false
		m.filter = ""
		m.cursor = 0
	case key.Matches(msg, m.keys.FilterDelete):
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
			m.cursor = 0
		}
	default:
		// Text is non-empty only for printable keys, so this ignores stray
		// control keys (arrows, etc.) rather than inserting garbage.
		if msg.Text != "" {
			m.filter += msg.Text
			m.cursor = 0
		}
	}
	return m, nil
}

// cycleSort advances to the next sort mode, keeping the cursor on the same repo
// across the reorder.
func (m Model) cycleSort() Model {
	var sel string
	if row, ok := m.currentRow(); ok {
		sel = row.Name
	}
	m.sortMode = (m.sortMode + 1) % sortModeCount
	m.cursor = m.indexOf(sel)
	return m
}

// actionRepo names the repo that repo-scoped keys (shell, refresh) act on: the
// pinned repo in the detail view, otherwise the selected row in the list.
func (m Model) actionRepo() (string, bool) {
	if m.view == detailView {
		if row, ok := m.detailRow(); ok {
			return row.Name, true
		}
		return "", false
	}
	if row, ok := m.currentRow(); ok {
		return row.Name, true
	}
	return "", false
}

func (m Model) handleDetailKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		m.view = listView
	case key.Matches(msg, m.keys.Up):
		if m.snapCursor > 0 {
			m.snapCursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.snapCursor < m.snapCount()-1 {
			m.snapCursor++
		}
	case key.Matches(msg, m.keys.Enter):
		// Enter on a snapshot shells in with that snapshot's context.
		if snap := m.selectedSnapshot(); snap != nil {
			if cmd := m.openShellCmd(snap); cmd != nil {
				m.statusMsg = ""
				return m, cmd
			}
		}
	}
	return m, nil
}

// openShellCmd builds the command that drops the user into a shell scoped to the
// currently selected repo (and snap, when non-nil). It returns nil only when
// there is no repo to act on. A failure to prepare the session is reported
// asynchronously as a shellExitedMsg so the error surfaces in the footer rather
// than being swallowed.
func (m Model) openShellCmd(snap *model.Snapshot) tea.Cmd {
	name, ok := m.actionRepo()
	if !ok {
		return nil
	}
	sess, err := m.app.ShellSession(name, snap)
	if err != nil {
		return func() tea.Msg { return shellExitedMsg{err: err} }
	}
	args := sess.InteractiveArgs()
	c := exec.Command(args[0], args[1:]...)
	c.Env = sess.Env
	// tea.ExecProcess drops out of the alt-screen, attaches the child to the
	// real terminal, and restores the TUI on exit. Cleanup removes the temp
	// password file once the shell is gone.
	return tea.ExecProcess(c, func(err error) tea.Msg {
		_ = sess.Cleanup()
		return shellExitedMsg{err: err}
	})
}

func (m Model) applyShellExit(msg shellExitedMsg) Model {
	if msg.err != nil {
		m.statusMsg = "shell: " + firstLine(msg.err.Error())
	}
	return m
}

// startRefresh marks a repo pending and returns its refresh command, or nil if a
// refresh is already in flight for it.
func (m Model) startRefresh(name string) tea.Cmd {
	if m.pending[name] {
		return nil
	}
	m.pending[name] = true
	return m.refreshCmd(name)
}

func (m Model) refreshCmd(name string) tea.Cmd {
	return func() tea.Msg {
		// Bound concurrency so a refresh-all doesn't spawn one restic process
		// per repo at once and trip Hetzner's 503 throttling (plan §10). Never
		// block past quit: a cancelled ctx abandons the queued slot.
		select {
		case m.sem <- struct{}{}:
		case <-m.ctx.Done():
			return nil
		}
		defer func() { <-m.sem }()
		row, err := m.app.RefreshRow(m.ctx, name)
		return repoRefreshedMsg{name: name, row: row, err: err}
	}
}

func (m Model) applyRefresh(msg repoRefreshedMsg) Model {
	// A refresh can change LastSnapshot/TotalSize and thus reorder the visible
	// list under a staleness/size sort. Anchor the cursor to the repo it was on
	// (by name) so the selection — and the repo r/s/enter act on — never silently
	// jumps, matching cycleSort's behavior.
	var selected string
	if row, ok := m.currentRow(); ok {
		selected = row.Name
	}
	delete(m.pending, msg.name)
	for i := range m.rows {
		if m.rows[i].Name == msg.name {
			m.rows[i] = msg.row
			break
		}
	}
	m.cursor = m.indexOf(selected)
	if msg.err != nil {
		// RefreshRow's error is a cache-persistence failure only; it carries no
		// secrets (it comes from the filesystem, not restic or secrets_command).
		m.statusMsg = "cache write failed: " + firstLine(msg.err.Error())
	}
	return m
}
