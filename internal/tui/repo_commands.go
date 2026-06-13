package tui

import (
	"os/exec"

	"resticscope/internal/app"
	"resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
)

// shellSnap returns the snapshot the Shell key scopes to for the active view.
// Detail uses the highlighted snapshot; list has no snapshot scope. Browse owns
// its own s handling (handleBrowseKey) and never reaches this. A detail view
// with zero cached snapshots also returns nil here — selectedSnapshot returns
// nil for an empty list — and that nil is intentional: the active scope of an
// empty detail view is the repo itself, so s falls through to a repo-only
// shell, matching what s does from the list view.
func (m Model) shellSnap() *model.Snapshot {
	if m.view == detailView {
		return m.selectedSnapshot()
	}
	return nil
}

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
	return shellCmdFromSession(sess)
}

// shellCmdFromSession builds the tea.Cmd that suspends the TUI, runs the prepared
// shell session via tea.ExecProcess, and runs Cleanup once the child exits. It
// honors sess.Dir when set, so the same builder serves both the repo/snapshot
// shell (openShellCmd, no Dir) and the extract success-view local shell
// (LocalShellSession, Dir = the extracted directory or its nearest enterable
// ancestor).
func shellCmdFromSession(sess *app.ShellSession) tea.Cmd {
	args := sess.InteractiveArgs()
	c := exec.Command(args[0], args[1:]...) //nolint:gosec // args are the internally-built shell invocation; exec.Command runs no shell, so there is no injection vector
	c.Env = sess.InteractiveEnv()
	if sess.Dir != "" {
		c.Dir = sess.Dir
	}
	return tea.ExecProcess(c, shellExitCallback(sess))
}

// shellExitCallback is the tea.ExecProcess completion callback. It runs the
// session Cleanup (which removes the 0600 temp password file in file mode) and
// reports both the shell's exit error and any cleanup failure. The `exec`
// subcommand can discard this same error because it has no long-lived UI surface
// to report it on and exits right after its deferred Cleanup runs; the TUI keeps
// running for hours after the shell returns, so a failed removal must not be
// swallowed — it would leave a plaintext restic password on disk (the temp file
// is removed by Cleanup, not on process exit) for the rest of the session.
// Pulled out of shellCmdFromSession so the cleanup-error wiring is unit-testable
// without the bubbletea runtime.
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
