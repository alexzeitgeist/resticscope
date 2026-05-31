package tui

import (
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/model"
)

// actionRepo names the repo that repo-scoped keys (shell, refresh) act on: the
// repo being browsed in the browse view, the pinned repo in the detail view,
// otherwise the selected row in the list.
func (m Model) actionRepo() (string, bool) {
	if m.view == browseView {
		if m.browseRepo != "" {
			return m.browseRepo, true
		}
		return "", false
	}
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
	// A refresh can change LastSnapshot and thus reorder the visible list under a
	// non-config sort. Anchor the cursor to the repo it was on (by name) so the
	// selection — and the repo r/s/enter act on — never silently jumps, matching
	// cycleSort's behavior.
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
