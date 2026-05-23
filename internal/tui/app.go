// Package tui is resticscope's Bubble Tea front end. It is a thin
// renderer/controller over the headless internal/app core: it holds no domain
// logic of its own, only view state (cursor, which repos are refreshing) and
// the commands that drive app.RefreshRow concurrently. Per the engineering
// rules it is tested via state transitions, not pixels.
package tui

import (
	"context"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
)

// Model is the root Bubble Tea model for the list view.
type Model struct {
	app       *app.App
	ctx       context.Context    // scopes in-flight refreshes; cancelled on quit
	cancel    context.CancelFunc // cancels ctx so quitting kills any running restic
	keys      keyMap
	styles    styles
	help      help.Model
	spinner   spinner.Model
	rows      []app.RepoStatus
	meta      map[string]rowMeta
	cursor    int
	pending   map[string]bool // repo name -> a refresh is in flight
	sem       chan struct{}   // bounds concurrent refreshes to parallelism
	resticVer string
	width     int
	statusMsg string // transient footer notice (e.g. a cache-save warning)
	quitting  bool
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
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		m.cancel() // stop any in-flight refresh so restic doesn't outlive the UI
		return m, tea.Quit
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
	case key.Matches(msg, m.keys.Refresh):
		if len(m.rows) > 0 {
			m.statusMsg = ""
			return m, m.startRefresh(m.rows[m.cursor].Name)
		}
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
	return m, nil
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
	delete(m.pending, msg.name)
	for i := range m.rows {
		if m.rows[i].Name == msg.name {
			m.rows[i] = msg.row
			break
		}
	}
	if msg.err != nil {
		// RefreshRow's error is a cache-persistence failure only; it carries no
		// secrets (it comes from the filesystem, not restic or secrets_command).
		m.statusMsg = "cache write failed: " + firstLine(msg.err.Error())
	}
	return m
}
