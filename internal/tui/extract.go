package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Extraction is a self-contained modal spanning review, execution, and terminal
// states. It owns cancellation, progress, target picking, and staging cleanup;
// the root zeroes all path-bearing state when the modal closes.

// extractState is the modal's state machine.
type extractState int

const (
	extractStateReview     extractState = iota // initial screen, before any restic call
	extractStateRunning                        // live restore in flight (file or directory)
	extractStateSuccess                        // post-rename
	extractStateCanceled                       // includes context.DeadlineExceeded
	extractStateError                          // any non-cancel error from App.Extract
	extractStateFilePicker                     // overlay over review, picking a target root
	extractStateKeepDelete                     // overlay over canceled / error, staging keep-or-delete prompt
)

// extractProgressBuffer bounds non-blocking progress so the restore callback
// cannot stall behind UI rendering.
const extractProgressBuffer = 64

// extractDriver provides extraction, local shell, privilege, and indexed-count
// operations without exposing their mechanisms to the modal.
type extractDriver interface {
	Extract(ctx context.Context, req app.ExtractRequest, onProgress func(app.ExtractProgress)) (app.ExtractResult, error)
	LocalShellSession(dir string) (*app.ShellSession, error)
	PrivilegedExtractProbe(ctx context.Context) error
	PrivilegedAuthCommand() *exec.Cmd
	// SubtreeCounts reports indexed descendants; known is false before commit.
	SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error)
}

// extractModel owns one extraction modal session.
type extractModel struct {
	drv extractDriver
	cfg config.Extract

	// parentCtx makes program quit cascade into per-operation cancellation.
	parentCtx context.Context

	state extractState

	// req is the active request; target picking updates TargetRoot.
	req app.ExtractRequest

	// ranTargetRoot records only a target actually dispatched, including failed or
	// cancelled runs; picker-only choices are forgotten.
	ranTargetRoot string

	// queue holds remaining diff sides in execution order.
	queue []app.ExtractRequest

	// published records diff sides completed before success, failure, or cancel.
	published []extractSideResult

	// diff holds modal-lifetime metadata for a directional pair; nil means plain extract.
	diff *extractDiffMeta

	// srcSize is display-only source size and is dropped with the modal.
	srcSize int64

	// srcFiles and srcDirs are advisory indexed counts shown only when known.
	srcFiles, srcDirs int
	srcCountsKnown    bool

	// isTargetBusy is an advisory preflight; the run-time check remains authoritative.
	isTargetBusy bool

	// targetFree is advisory free space, re-probed after retargeting and never a gate.
	targetFree      int64
	targetFreeKnown bool

	// staging and final are derived from req and updated with TargetRoot.
	staging string
	final   string

	// gen and cancel reject superseded work. Because gen restarts with each modal,
	// root Update also rejects extract messages while the modal is inactive.
	gen    int
	cancel context.CancelFunc

	progressCh chan app.ExtractProgress

	result app.ExtractResult
	err    error

	// stagingExists is probed on transitions and acted-on keys, never during View,
	// because Lstat on an automounted target may block.
	stagingExists bool

	// progress and rate hold the latest monotonic running sample.
	progress app.ExtractProgress
	rate     rateSampler

	// filepicker is built lazily so disk is read only when target picking opens.
	filepicker     filepicker.Model
	filepickerInit bool
	filepickerErr  string
	pickerStyles   filepicker.Styles

	// pickerModeW is scanned asynchronously across the current directory so mode
	// columns do not shift as rows scroll; pickerModeDir detects navigation.
	pickerModeW   int
	pickerModeDir string

	// isSudoBusy debounces review while privilege probing or authentication runs.
	isSudoBusy bool

	// reviewNotice is a path-free, one-line review status.
	reviewNotice string

	// noticeAfterClose surfaces modal errors after return.
	noticeAfterClose string

	// height sizes the lazily built file-picker viewport from the live terminal.
	height int
}

// ErrExtractUnsupportedType reports a browse entry that cannot be extracted.
var ErrExtractUnsupportedType = errors.New("extract: source type not supported in v1")

// seedTargetMemo applies the last dispatched target unless the request overrides it.
func (m Model) seedTargetMemo(req app.ExtractRequest) app.ExtractRequest {
	if req.TargetRoot == "" {
		req.TargetRoot = m.extractTargetMemo
	}
	return req
}

// newExtractModel plans an explicit request and preflights advisory target state.
// Invalid requests return before opening the modal; the file picker remains lazy.
func newExtractModel(a *app.App, parentCtx context.Context, req app.ExtractRequest, srcSize int64) (extractModel, error) {
	staging, final, err := app.PlanExtractPaths(a.Cfg.Extract, req)
	if err != nil {
		return extractModel{}, err
	}
	free, freeKnown := app.ExtractFreeSpace(staging)
	return extractModel{
		drv:          a,
		cfg:          a.Cfg.Extract,
		pickerStyles: filepickerStyles(a.Cfg.Theme.Palette()),
		parentCtx:    parentCtx,
		state:        extractStateReview,
		req:          req,
		srcSize:      srcSize,
		staging:      staging,
		final:        final,
		// Match the preflight cost paid after file-picker selection.
		isTargetBusy:    isExtractRefusal(app.FreshTargetCheck(staging, final)),
		targetFree:      free,
		targetFreeKnown: freeKnown,
	}, nil
}

// extractDiffMeta describes a directional pair, filtered side counts, and its
// published container.
type extractDiffMeta struct {
	firstShort, secondShort string
	filters                 model.ModifierKind
	sourceIsDir             bool
	firstCount, secondCount int
	containerDir            string
}

// extractSideResult pairs a published side with its snapshot short ID.
type extractSideResult struct {
	short  string
	result app.ExtractResult
}

// newExtractDiffModel plans every non-empty side before opening and preflights
// their shared target, preventing mid-run planning failures.
func newExtractDiffModel(a *app.App, parentCtx context.Context, reqs []app.ExtractRequest, meta extractDiffMeta) (extractModel, error) {
	if len(reqs) == 0 {
		return extractModel{}, errors.New("nothing to extract")
	}
	var staging0, final0 string
	busy := false
	for i, r := range reqs {
		staging, final, err := app.PlanExtractPaths(a.Cfg.Extract, r)
		if err != nil {
			return extractModel{}, err
		}
		if i == 0 {
			staging0, final0 = staging, final
		}
		if isExtractRefusal(app.FreshTargetCheck(staging, final)) {
			busy = true
		}
	}
	containerDir, err := app.ExtractDiffTargetDir(a.Cfg.Extract, reqs[0])
	if err != nil {
		return extractModel{}, err
	}
	meta.containerDir = containerDir
	// The first staging path identifies the shared target filesystem.
	free, freeKnown := app.ExtractFreeSpace(staging0)
	return extractModel{
		drv:             a,
		cfg:             a.Cfg.Extract,
		pickerStyles:    filepickerStyles(a.Cfg.Theme.Palette()),
		parentCtx:       parentCtx,
		state:           extractStateReview,
		req:             reqs[0],
		queue:           reqs[1:],
		diff:            &meta,
		staging:         staging0,
		final:           final0,
		isTargetBusy:    busy,
		targetFree:      free,
		targetFreeKnown: freeKnown,
	}, nil
}

// supersede advances generation and cancels in-flight operation context.
func (m *extractModel) supersede() {
	m.gen++
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// extractCountsMsg carries advisory indexed directory counts.
type extractCountsMsg struct {
	gen         int
	files, dirs int
	known       bool
}

// countsCmd queries contained counts for indexed directory sources. Files,
// invalid models, and diff extracts need no lookup.
func (m *extractModel) countsCmd() tea.Cmd {
	if m.drv == nil || m.diff != nil || m.req.Mode != app.ExtractDirectoryTree {
		return nil
	}
	gen, drv, ctx := m.gen, m.drv, m.parentCtx
	repo, snap, src := m.req.Repo, m.req.SnapshotID, m.req.Source
	return func() tea.Msg {
		files, dirs, known, err := drv.SubtreeCounts(ctx, repo, snap, src)
		if err != nil {
			known = false
		}
		return extractCountsMsg{gen: gen, files: files, dirs: dirs, known: known}
	}
}

// applyCounts installs known counts only for the current generation.
func (m *extractModel) applyCounts(msg extractCountsMsg) {
	if msg.gen != m.gen || !msg.known {
		return
	}
	m.srcFiles, m.srcDirs, m.srcCountsKnown = msg.files, msg.dirs, true
}

// extractRunDoneMsg carries the result of a live run.
type extractRunDoneMsg struct {
	gen    int
	result app.ExtractResult
	err    error
}

// extractProgressMsg is one sampled progress tick.
type extractProgressMsg struct {
	gen      int
	progress app.ExtractProgress
}

// extractDeleteStagingDoneMsg carries the result of a user-confirmed staging
// delete from the keep-or-delete prompt.
type extractDeleteStagingDoneMsg struct {
	gen int
	err error
}

// extractSudoProbeMsg carries the sudo readiness probe result for a privileged
// commit from review.
type extractSudoProbeMsg struct {
	gen int
	err error
}

// extractSudoAuthMsg carries the outcome of the interactive `sudo -v` run via
// tea.ExecProcess (the TUI suspends, sudo prompts on the real TTY, resumes).
type extractSudoAuthMsg struct {
	gen int
	err error
}

// extractBackToBrowseMsg asks the root to restore the origin and drop the modal.
type extractBackToBrowseMsg struct {
	notice string
}

// startRun dispatches extraction and its progress pump.
func (m *extractModel) startRun() tea.Cmd {
	m.supersede()
	runCtx, cancel := context.WithCancel(m.parentCtx)
	m.cancel = cancel
	gen := m.gen
	req := m.req
	m.ranTargetRoot = req.TargetRoot
	drv := m.drv

	progress := make(chan app.ExtractProgress, extractProgressBuffer)
	m.progressCh = progress
	m.rate = rateSampler{}
	m.progress = app.ExtractProgress{}

	runCmd := func() tea.Msg {
		res, err := drv.Extract(runCtx, req, func(p app.ExtractProgress) {
			select {
			case progress <- p:
			default:
			}
		})
		close(progress)
		return extractRunDoneMsg{gen: gen, result: res, err: err}
	}
	return tea.Batch(runCmd, waitForExtractProgress(gen, progress))
}

// waitForExtractProgress emits the next progress sample.
func waitForExtractProgress(gen int, progress <-chan app.ExtractProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-progress
		if !ok {
			return nil
		}
		return extractProgressMsg{gen: gen, progress: p}
	}
}

// ensureFilepicker lazily starts at the request override, configured root, or home.
func (m *extractModel) ensureFilepicker() tea.Cmd {
	if m.filepickerInit {
		return nil
	}
	m.filepickerInit = true
	fp := filepicker.New()
	fp.Styles = m.pickerStyles
	fp.Cursor = "▎"
	fp.DirAllowed = true
	fp.FileAllowed = false
	// Hidden directories may be valid or automounted targets.
	fp.ShowHidden = true
	fp.ShowPermissions = true
	fp.ShowSize = true
	// AutoHeight is off because the picker is embedded under our own header and
	// footer, so we size it here and again on every WindowSizeMsg.
	fp.AutoHeight = false
	fp.SetHeight(extractFilePickerHeight(m.height))
	// A session override wins so repicking begins at the last dispatched target.
	dir := m.req.TargetRoot
	if dir == "" {
		dir = m.cfg.TargetRoot
	}
	if dir == "" || !filepath.IsAbs(dir) {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = "/"
		}
	}
	fp.CurrentDirectory = dir
	m.filepicker = fp
	return tea.Batch(m.filepicker.Init(), m.syncPickerModeWidth())
}

// syncPickerModeWidth asynchronously scans new directories so automounts cannot
// stall Update; visible rows provide the width until the result arrives.
func (m *extractModel) syncPickerModeWidth() tea.Cmd {
	dir := m.filepicker.CurrentDirectory
	if dir == m.pickerModeDir {
		return nil
	}
	m.pickerModeDir = dir
	m.pickerModeW = 0
	return pickerModeWidthCmd(dir)
}

// extractPickerModeWidthMsg tags a width result with its directory for stale rejection.
type extractPickerModeWidthMsg struct {
	dir string
	w   int
}

// pickerModeWidthCmd finds the widest mode across all visible and hidden entries.
// Read failures preserve the on-screen fallback width.
func pickerModeWidthCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		out := extractPickerModeWidthMsg{dir: dir}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return out
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if n := len(info.Mode().String()); n > out.w {
				out.w = n
			}
		}
		return out
	}
}

// extractFilePickerHeight reserves modal chrome and provides a pre-resize fallback.
func extractFilePickerHeight(termHeight int) int {
	if h := termHeight - 8; h > 1 {
		return h
	}
	return 10
}

// setHeight synchronizes the manually sized file-picker viewport.
func (m *extractModel) setHeight(h int) {
	m.height = h
	if m.filepickerInit {
		m.filepicker.SetHeight(extractFilePickerHeight(h))
	}
}

// updateFilePicker forwards asynchronous directory messages required to populate
// the embedded picker.
func (m extractModel) updateFilePicker(msg tea.Msg) (extractModel, tea.Cmd) {
	if wm, ok := msg.(extractPickerModeWidthMsg); ok {
		// Reject width scans that raced picker navigation.
		if wm.dir == m.filepicker.CurrentDirectory {
			m.pickerModeW = wm.w
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.filepicker, cmd = m.filepicker.Update(msg)
	return m, tea.Batch(cmd, m.syncPickerModeWidth())
}

// applyRunDone installs a result and advances queued diff sides before success.
func (m *extractModel) applyRunDone(msg extractRunDoneMsg) tea.Cmd {
	if msg.gen != m.gen {
		return nil
	}
	m.result = msg.result
	if msg.err != nil {
		m.err = msg.err
		if isExtractCancelErr(msg.err) {
			m.state = extractStateCanceled
		} else {
			m.state = extractStateError
		}
		// Cache staging state for render; acted-on terminal keys re-probe it.
		m.stagingExists = m.stagingPending()
		return nil
	}
	m.err = nil
	if m.diff != nil {
		m.published = append(m.published, extractSideResult{short: m.req.SnapshotShort, result: msg.result})
		if len(m.queue) > 0 {
			return m.advanceQueue()
		}
	}
	m.state = extractStateSuccess
	return nil
}

// advanceQueue starts the next diff side with shared privilege and target choices.
// Unexpected replanning failure degrades to the error screen.
func (m *extractModel) advanceQueue() tea.Cmd {
	next := m.queue[0]
	next.Privileged = m.req.Privileged
	next.TargetRoot = m.req.TargetRoot
	staging, final, err := app.PlanExtractPaths(m.cfg, next)
	if err != nil {
		m.err = err
		m.state = extractStateError
		m.stagingExists = false
		return nil
	}
	m.queue = m.queue[1:]
	m.req = next
	m.staging, m.final = staging, final
	cmd := m.startRun()
	m.state = extractStateRunning
	return cmd
}

// applyProgress merges fields monotonically and advances rate only with byte growth.
func (m *extractModel) applyProgress(msg extractProgressMsg) tea.Cmd {
	if msg.gen != m.gen {
		return nil
	}
	p := msg.progress
	if p.BytesDone > m.progress.BytesDone {
		m.progress.BytesDone = p.BytesDone
		m.rate.update(p.BytesDone, time.Now())
	}
	if p.BytesTotal > m.progress.BytesTotal {
		m.progress.BytesTotal = p.BytesTotal
	}
	if p.FilesDone > m.progress.FilesDone {
		m.progress.FilesDone = p.FilesDone
	}
	if p.FilesTotal > m.progress.FilesTotal {
		m.progress.FilesTotal = p.FilesTotal
	}
	if p.SecondsElapsed > m.progress.SecondsElapsed {
		m.progress.SecondsElapsed = p.SecondsElapsed
	}
	return waitForExtractProgress(msg.gen, m.progressCh)
}

// applyDeleteStagingDone closes through the common path and surfaces a path-free
// deletion error after return.
func (m *extractModel) applyDeleteStagingDone(msg extractDeleteStagingDoneMsg) tea.Cmd {
	if msg.gen != m.gen {
		return nil
	}
	if msg.err != nil {
		m.noticeAfterClose = firstLine(msg.err.Error())
	}
	return returnExtract(m.noticeAfterClose)
}

// back centralizes cancellation, picker closing, and modal exit for q and escape.
func (m extractModel) back() (extractModel, tea.Cmd, bool) {
	switch m.state {
	case extractStateRunning:
		// The worker reports cancellation and transitions through applyRunDone.
		if m.cancel != nil {
			m.cancel()
		}
		return m, nil, false
	case extractStateFilePicker:
		m.state = extractStateReview
		m.filepickerErr = ""
		return m, nil, false
	case extractStateCanceled, extractStateError, extractStateKeepDelete,
		extractStateReview, extractStateSuccess:
		// Supersede before notifying the root so in-flight privilege results cannot
		// commit after the user exits.
		m.supersede()
		return m, returnExtract(m.noticeAfterClose), true
	}
	return m, nil, false
}

// handleKey returns updated modal state, work, and whether the root should close it.
func (m extractModel) handleKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	switch m.state {
	case extractStateReview:
		return m.handleReviewKey(keys, msg)
	case extractStateRunning:
		return m.handleRunningKey(keys, msg)
	case extractStateSuccess:
		return m.handleSuccessKey(keys, msg)
	case extractStateCanceled, extractStateError:
		return m.handleTerminalKey(keys, msg)
	case extractStateFilePicker:
		return m.handleFilePickerKey(keys, msg)
	case extractStateKeepDelete:
		return m.handleKeepDeleteKey(keys, msg)
	}
	return m, nil, false
}

func (m extractModel) handleReviewKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	// During privilege checks, only escape remains active.
	if m.isSudoBusy && !key.Matches(msg, keys.Back) {
		return m, nil, false
	}
	switch {
	case key.Matches(msg, keys.Back):
		return m.back()
	case key.Matches(msg, keys.Target):
		m.state = extractStateFilePicker
		m.filepickerErr = ""
		m.reviewNotice = ""
		cmd := m.ensureFilepicker()
		return m, cmd, false
	case key.Matches(msg, keys.Priv):
		// Restoring snapshot ownership requires root; readiness is checked on commit.
		m.req.Privileged = !m.req.Privileged
		m.reviewNotice = ""
		return m, nil, false
	case key.Matches(msg, keys.Enter):
		m.reviewNotice = ""
		if m.req.Privileged {
			// Probe cached sudo authorization in its own generation before dispatch.
			m.supersede()
			m.isSudoBusy = true
			return m, m.sudoProbeCmd(), false
		}
		// Plain commits run directly without a dry-run preview.
		cmd := m.commitRun()
		return m, cmd, false
	}
	return m, nil, false
}

// commitRun dispatches plain, probed, and interactively authorized commits.
func (m *extractModel) commitRun() tea.Cmd {
	m.isSudoBusy = false
	cmd := m.startRun()
	m.state = extractStateRunning
	return cmd
}

// sudoProbeCmd checks whether privilege escalation can proceed without prompting.
func (m *extractModel) sudoProbeCmd() tea.Cmd {
	gen := m.gen
	drv := m.drv
	ctx := m.parentCtx
	return func() tea.Msg {
		return extractSudoProbeMsg{gen: gen, err: drv.PrivilegedExtractProbe(ctx)}
	}
}

// applySudoProbe commits when ready, rejects unavailable privilege, or suspends
// the TUI for interactive authentication before a non-interactive helper run.
func (m *extractModel) applySudoProbe(msg extractSudoProbeMsg) tea.Cmd {
	if msg.gen != m.gen || m.state != extractStateReview || !m.isSudoBusy {
		return nil
	}
	if msg.err == nil {
		return m.commitRun()
	}
	authCmd := m.drv.PrivilegedAuthCommand()
	if errors.Is(msg.err, app.ErrPrivilegedExtractUnavailable) || authCmd == nil {
		m.isSudoBusy = false
		m.reviewNotice = "privileged extract not available"
		return nil
	}
	gen := m.gen
	return tea.ExecProcess(authCmd, func(err error) tea.Msg {
		return extractSudoAuthMsg{gen: gen, err: err}
	})
}

// applySudoAuth commits after authentication or returns to review with a path-free notice.
func (m *extractModel) applySudoAuth(msg extractSudoAuthMsg) tea.Cmd {
	if msg.gen != m.gen || m.state != extractStateReview || !m.isSudoBusy {
		return nil
	}
	if msg.err != nil {
		m.isSudoBusy = false
		m.reviewNotice = "sudo authentication failed — cannot extract as root"
		return nil
	}
	return m.commitRun()
}

func (m extractModel) handleRunningKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) {
		return m.back()
	}
	return m, nil, false
}

func (m extractModel) handleSuccessKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Back):
		return m.back()
	case key.Matches(msg, keys.Shell):
		// Open a credential-free shell at the result or diff container. Path-free
		// setup errors return through shellExitedMsg, leaving success visible.
		dir := m.result.FinalDir
		if m.diff != nil {
			dir = m.diff.containerDir
		}
		sess, err := m.drv.LocalShellSession(dir)
		if err != nil {
			return m, func() tea.Msg { return shellExitedMsg{err: err} }, false
		}
		return m, shellCmdFromSession(sess), false
	}
	return m, nil, false
}

// handleTerminalKey offers cleanup only while staging still exists. Acted-on
// keys re-probe it, while ignored keys avoid potentially blocking automount I/O.
func (m extractModel) handleTerminalKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if !key.Matches(msg, keys.Delete) && !key.Matches(msg, keys.Keep) &&
		!key.Matches(msg, keys.Back) &&
		!key.Matches(msg, keys.Target) {
		return m, nil, false
	}
	m.stagingExists = m.stagingPending()
	if m.stagingExists {
		switch {
		case key.Matches(msg, keys.Delete):
			m.state = extractStateKeepDelete
			return m, m.deleteStagingCmd(), false
		case key.Matches(msg, keys.Keep),
			key.Matches(msg, keys.Back):
			return m.back()
		}
		return m, nil, false
	}
	switch {
	case isExtractRefusal(m.err) && key.Matches(msg, keys.Target) && len(m.published) == 0:
		// Retarget only before any diff side publishes, preventing split roots.
		m.err = nil
		m.result = app.ExtractResult{}
		m.state = extractStateFilePicker
		m.filepickerErr = ""
		cmd := m.ensureFilepicker()
		return m, cmd, false
	case key.Matches(msg, keys.Back):
		return m.back()
	}
	return m, nil, false
}

// handleKeepDeleteKey allows exit while staging deletion completes asynchronously.
func (m extractModel) handleKeepDeleteKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) {
		// Superseding on exit discards the in-flight deletion result.
		return m.back()
	}
	return m, nil, false
}

// handleFilePickerKey forwards input and replans selected roots, refusing occupied paths.
func (m extractModel) handleFilePickerKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) {
		// Intercept escape before the picker's own parent-directory binding.
		return m.back()
	}
	var cmd tea.Cmd
	m.filepicker, cmd = m.filepicker.Update(msg)
	cmd = tea.Batch(cmd, m.syncPickerModeWidth())
	if didSelect, fppath := m.filepicker.DidSelectFile(msg); didSelect {
		if newReq, staging, final, perr := m.planOverrideAllSides(fppath); perr == nil {
			m.req = newReq
			m.staging = staging
			m.final = final
			m.state = extractStateReview
			m.filepickerErr = ""
			// The accepted root is fresh; probe its filesystem for advisory space.
			m.isTargetBusy = false
			m.targetFree, m.targetFreeKnown = app.ExtractFreeSpace(staging)
		} else {
			m.filepickerErr = firstLine(perr.Error())
		}
	} else if didSelect, _ := m.filepicker.DidSelectDisabledFile(msg); didSelect {
		m.filepickerErr = "select a directory"
	}
	return m, cmd, false
}

// planOverrideAllSides retargets every diff side atomically, refusing any occupied
// path so later sides cannot fail after an earlier side publishes.
func (m *extractModel) planOverrideAllSides(root string) (app.ExtractRequest, string, string, error) {
	newReq, staging, final, err := planExtractOverride(m.cfg, m.req, root)
	if err != nil {
		return app.ExtractRequest{}, "", "", err
	}
	newQueue := make([]app.ExtractRequest, len(m.queue))
	for i, q := range m.queue {
		nq, _, _, qerr := planExtractOverride(m.cfg, q, root)
		if qerr != nil {
			return app.ExtractRequest{}, "", "", qerr
		}
		newQueue[i] = nq
	}
	m.queue = newQueue
	if m.diff != nil {
		dir, derr := app.ExtractDiffTargetDir(m.cfg, newReq)
		if derr != nil {
			return app.ExtractRequest{}, "", "", derr
		}
		m.diff.containerDir = dir
	}
	return newReq, staging, final, nil
}

// planExtractOverride uses the run-time occupancy check and its path-free errors
// when replanning under a chosen root.
func planExtractOverride(cfg config.Extract, base app.ExtractRequest, root string) (app.ExtractRequest, string, string, error) {
	req := base
	req.TargetRoot = root
	staging, final, err := app.PlanExtractPaths(cfg, req)
	if err != nil {
		return base, "", "", err
	}
	if err := app.FreshTargetCheck(staging, final); err != nil {
		return base, "", "", err
	}
	return req, staging, final, nil
}

// deleteStagingCmd removes only the run-reported staging path after verifying it
// matches the planned path; deletion errors remain path-free.
func (m extractModel) deleteStagingCmd() tea.Cmd {
	gen := m.gen
	staging := m.result.StagingDir
	expected := m.staging
	return func() tea.Msg {
		if staging == "" || staging != expected {
			return extractDeleteStagingDoneMsg{gen: gen, err: errors.New("staging path mismatch; refusing delete")}
		}
		return extractDeleteStagingDoneMsg{gen: gen, err: app.DeleteExtractStaging(staging)}
	}
}

// stagingPending reports whether this run's staging directory still exists.
func (m extractModel) stagingPending() bool {
	return m.result.StagingCreated && stagingDirExists(m.result.StagingDir)
}

// stagingDirExists avoids filesystem access for an empty path.
func stagingDirExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// isExtractCancelErr reports direct or wrapped cancellation and deadline errors.
func isExtractCancelErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isExtractRefusal reports occupied-path failures that retargeting can remedy.
func isExtractRefusal(err error) bool {
	return errors.Is(err, app.ErrExtractFinalExists) || errors.Is(err, app.ErrExtractStagingExists)
}

// returnExtract asks the root to switch views and clear the modal.
func returnExtract(notice string) tea.Cmd {
	return func() tea.Msg {
		return extractBackToBrowseMsg{notice: notice}
	}
}

// extractRequestFromBrowseEntry builds a file or directory request and rejects
// symlinks and special nodes; the app layer revalidates the type attestation.
func extractRequestFromBrowseEntry(repo, snapID string, entry model.BrowseEntry) (app.ExtractRequest, error) {
	if len(snapID) < 8 {
		return app.ExtractRequest{}, errors.New("snapshot id too short")
	}
	snapShort := snapID[:8]
	source := model.CleanBrowsePath(entry.Path)
	var mode app.ExtractMode
	var wasFile bool
	switch {
	case entry.Type == model.NodeTypeFile:
		// File extraction preserves its mirrored path below the snapshot directory.
		mode = app.ExtractFile
		wasFile = true
	case entry.Type == model.NodeTypeDir || entry.IsDir:
		mode = app.ExtractDirectoryTree
	default:
		return app.ExtractRequest{}, ErrExtractUnsupportedType
	}
	name, err := app.SanitizeExtractSlug(path.Base(source))
	if err != nil {
		return app.ExtractRequest{}, err
	}
	return app.ExtractRequest{
		Repo:           repo,
		SnapshotID:     snapID,
		SnapshotShort:  snapShort,
		Source:         source,
		SourceName:     name,
		Mode:           mode,
		WasRegularFile: wasFile,
	}, nil
}

// extractRequestFromFindVersion reuses browse path and slug rules for a grouped
// occurrence. Grouping rejects explicit non-files but permits untyped matches.
func extractRequestFromFindVersion(repo, snapID, p string) (app.ExtractRequest, error) {
	return extractRequestFromBrowseEntry(repo, snapID, model.BrowseEntry{Path: p, Type: model.NodeTypeFile})
}

// extractRequestFromSnapshot builds a root-source request whose final path is
// the repository's short-snapshot directory.
func extractRequestFromSnapshot(repo string, snap *model.Snapshot) (app.ExtractRequest, error) {
	if snap == nil || len(snap.ID) < 8 {
		return app.ExtractRequest{}, errors.New("snapshot id too short")
	}
	short := snap.ID[:8]
	return app.ExtractRequest{
		Repo:          repo,
		SnapshotID:    snap.ID,
		SnapshotShort: short,
		Source:        "/",
		SourceName:    short,
		Mode:          app.ExtractDirectoryTree,
	}, nil
}

// shortHelp returns footer bindings that mirror each modal state's handler.
func (m extractModel) shortHelp(keys keyMap) []key.Binding {
	switch m.state {
	case extractStateReview:
		return []key.Binding{helpAs(keys.Enter, "extract"), keys.Target, keys.Priv, keys.Back}
	case extractStateRunning:
		return []key.Binding{helpAs(keys.Back, "cancel")}
	case extractStateSuccess:
		// The modal may return to browse, detail, versions, or diff.
		return []key.Binding{helpAs(keys.Shell, "shell here"), keys.Back}
	case extractStateCanceled, extractStateError:
		if m.stagingExists {
			return []key.Binding{keys.Keep, keys.Delete}
		}
		if isExtractRefusal(m.err) && len(m.published) == 0 {
			// Retargeting is withheld after a diff side publishes.
			return []key.Binding{keys.Target, keys.Back}
		}
		return []key.Binding{keys.Back}
	case extractStateFilePicker:
		return []key.Binding{keys.Back}
	case extractStateKeepDelete:
		return []key.Binding{helpAs(keys.Back, "back")}
	}
	return nil
}
