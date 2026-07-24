package tui

import (
	"os/exec"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
)

// shellSnap returns the selected detail snapshot. Nil selects a repository-only
// shell from the list or an empty detail; browse handles shell commands itself.
func (m Model) shellSnap() *model.Snapshot {
	if m.view == detailView {
		return m.selectedSnapshot()
	}
	return nil
}

// actionRepo returns the repository pinned to browse or detail, or the selected
// list row.
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

// openShellCmd prepares a shell for the active repository and optional snapshot.
// It returns nil without a repository and reports preparation failures through
// shellExitedMsg.
func (m Model) openShellCmd(snap *model.Snapshot) tea.Cmd {
	name, ok := m.actionRepo()
	if !ok {
		return nil
	}
	sess, err := m.app.ShellSession(name, snap)
	if err != nil {
		return func() tea.Msg { return shellExitedMsg{err: err} }
	}
	return shellCmdFromSession(sess)
}

// shellCmdFromSession suspends the TUI for a prepared shell and cleans up after
// the child exits. sess.Dir supports both repository and extracted-directory
// shells.
func shellCmdFromSession(sess *app.ShellSession) tea.Cmd {
	args := sess.InteractiveArgs()
	c := exec.Command(args[0], args[1:]...) //nolint:gosec // args are the internally-built shell invocation; exec.Command runs no shell, so there is no injection vector
	c.Env = sess.InteractiveEnv()
	if sess.Dir != "" {
		c.Dir = sess.Dir
	}
	return tea.ExecProcess(c, shellExitCallback(sess))
}

// shellExitCallback removes the session's 0600 temporary password file and
// reports both cleanup and child-process errors. Cleanup failure must remain
// visible because nothing else removes the plaintext password file.
func shellExitCallback(sess *app.ShellSession) func(error) tea.Msg {
	return func(err error) tea.Msg {
		return shellExitedMsg{err: err, cleanupErr: sess.Cleanup()}
	}
}

func (m Model) applyShellExit(msg shellExitedMsg) Model {
	switch {
	case msg.cleanupErr != nil:
		// A leftover password file is actionable (delete it); prefer it over the
		// shell's exit status, which the user usually triggered themselves.
		m.statusMsg = "shell cleanup: " + firstLine(msg.cleanupErr.Error())
	case msg.err != nil:
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

// refreshCmd waits for a semaphore slot, aborts if the model context closes,
// and reports the completed refresh asynchronously.
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
	// Refresh may reorder non-config sorts, so reanchor the cursor by repository
	// name.
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
	if msg.name == m.detailName {
		m = m.normalizeDetailMarks(m.snapDisplay())
	}
	m.cursor = m.indexOf(selected)
	if msg.err != nil {
		// RefreshRow's error is a cache-persistence failure only; it carries no
		// secrets (it comes from the filesystem, not restic or secrets_command).
		m.statusMsg = "cache write failed: " + firstLine(msg.err.Error())
	}
	return m
}
