package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// extract.go is the TUI sub-model for the extract feature: a self-contained Bubble
// Tea model that walks the user from a "review" screen straight to a "running"
// screen and then to success / cancel / error. It owns the per-op generation
// token and cancel context (mirroring browse.go), the embedded bubbles/filepicker
// for target-root override, the sampled-rate progress line, and the keep-or-delete
// overlay. The sub-model holds source / staging / final paths only for the
// lifetime of the modal — every exit path funnels through extractBackToBrowseMsg,
// where the root model replaces the whole sub-model with its zero value, so no
// path data lingers in the model (non-negotiable #1, same discipline as browse /
// find-versions / snapshot-diff).
//
// Browse wires the `e` key to construct this sub-model from the selected
// BrowseEntry via extractRequestFromBrowseEntry; detail wires its `e` to the
// whole-snapshot request (Source "/") via extractRequestFromSnapshot;
// find-versions wires its `e` to the selected version's newest occurrence via
// extractRequestFromFindVersion. The success-view `s` action opens a
// credential-free local shell rooted at the extracted directory via
// App.LocalShellSession.

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

// extractProgressBuffer bounds the progress channel. Sends are non-blocking, so
// the producer (the resticx restore event callback) never stalls behind a full
// UI channel. Mirrors browseProgressBuffer / diffProgressBuffer.
const extractProgressBuffer = 64

// extractDriver is the small consumer-side interface the sub-model needs from
// the app layer (engineering rule 3: tiny, defined where it is used). Satisfied
// by *app.App; the test double in extract_test.go implements it directly.
// LocalShellSession backs the success-view `s` action: a credential-free shell
// rooted at the extracted directory. PrivilegedExtractProbe backs the review
// `p` toggle's commit path: nil means a privileged helper launch will not
// prompt; on an error PrivilegedAuthCommand supplies the interactive
// authentication command (e.g. `sudo -v`) to run first — the elevation
// mechanism is the app layer's knowledge, never hardcoded here.
type extractDriver interface {
	Extract(ctx context.Context, req app.ExtractRequest, onProgress func(app.ExtractProgress)) (app.ExtractResult, error)
	LocalShellSession(dir string) (*app.ShellSession, error)
	PrivilegedExtractProbe(ctx context.Context) error
	PrivilegedAuthCommand() *exec.Cmd
}

// extractModel is the self-contained sub-model. Browse wiring constructs one per
// `e` press, the root Model hosts it on m.extract, and the routing layer
// dispatches keys / messages to it while m.view == extractView.
type extractModel struct {
	drv extractDriver
	cfg config.Extract

	// parentCtx scopes every per-op child context. It is the root model's
	// program-scoped context, so quitting the program still cascades into any
	// in-flight extract.
	parentCtx context.Context

	state extractState

	// req is the active request. enter dispatches a single live Extract for both
	// file and directory sources. TargetRoot is updated by the filepicker overlay.
	req app.ExtractRequest

	// srcSize is the source node's size from the originating BrowseEntry (a
	// directory's recursive subtree size, a file's byte length). Display-only:
	// shown on the review "Type" line; dropped with the rest of the sub-model on
	// exit so no size derived from a path lingers after leaving the modal.
	srcSize int64

	// staging / final are derived from req via PlanExtractPaths and updated
	// alongside req when the filepicker overlay changes TargetRoot.
	staging string
	final   string

	// gen + cancel mirror browse.go's discipline: every transition that starts
	// a new restic call bumps gen and supersedes the prior cancel, so a late
	// message from a canceled run is dropped by msg.gen check. Unlike browse —
	// whose gen lives on the persistent root Model and is never reset — this
	// sub-model is re-created per `e`, so gen restarts at 0 each session and is
	// only unique within one session. The root Update therefore also gates every
	// extract message on m.view == extractView, so a stale message from a prior
	// session is dropped while the user is outside the modal.
	gen    int
	cancel context.CancelFunc

	// progress channel is non-nil while running.
	progressCh chan app.ExtractProgress

	// result + err are the terminal outcomes of the current state.
	result app.ExtractResult
	err    error

	// stagingExists caches stagingPending() — whether this run's staging dir is
	// still on disk. Set when a run lands on a terminal state and re-probed per
	// terminal-state keypress, so the render path (shortHelp /
	// extractTerminalBody) never touches the filesystem: an Lstat against an
	// automounted target root can block, and View runs on every message.
	stagingExists bool

	// progress is the latest sampled snapshot during running; rate computes the
	// sampled bytes/sec for the running view (shared sampler with browse).
	progress app.ExtractProgress
	rate     rateSampler

	// filepicker is constructed lazily on first entry to extractStateFilePicker
	// so the embedded model only reads disk when the user opens it.
	filepicker     filepicker.Model
	filepickerInit bool
	filepickerErr  string

	// sudoBusy is true between committing a privileged extract and the sudo
	// probe / interactive auth resolving. It debounces enter on review and
	// gen-gates the probe/auth messages alongside gen itself.
	sudoBusy bool

	// reviewNotice is a one-line, path-free notice rendered on the review
	// screen (e.g. "sudo authentication failed"). Cleared on the next review
	// keypress that changes state.
	reviewNotice string

	// noticeAfterClose, when non-empty, is surfaced as the root model's status
	// line on the next return to browse (used by errors that survive the modal).
	noticeAfterClose string

	// height is synced from the root model's WindowSizeMsg so the lazily-built
	// filepicker viewport sizes from the live terminal. (Rendering itself reads
	// the root's size; this is for ensureFilepicker, which doesn't hold the Model.)
	height int
}

// ErrExtractUnsupportedType is the TUI-side sentinel returned by
// extractRequestFromBrowseEntry when the browse entry's type is neither "file"
// nor "dir" (symlink, device, fifo, socket). Browse wiring surfaces this on the
// status line when the user presses `e` on an unsupported row.
var ErrExtractUnsupportedType = errors.New("extract: source type not supported in v1")

// newExtractModel constructs the sub-model from an explicit request. It calls
// PlanExtractPaths to derive staging / final; an invalid request bubbles back
// to the caller, which then stays in browse and surfaces a status-line error
// rather than switching the view. srcSize is the originating
// BrowseEntry's size, shown read-only on the review screen. The filepicker is
// NOT initialized here — that happens lazily on first entry to
// extractStateFilePicker.
func newExtractModel(a *app.App, parentCtx context.Context, req app.ExtractRequest, srcSize int64) (extractModel, error) {
	staging, final, err := app.PlanExtractPaths(a.Cfg.Extract, req)
	if err != nil {
		return extractModel{}, err
	}
	return extractModel{
		drv:       a,
		cfg:       a.Cfg.Extract,
		parentCtx: parentCtx,
		state:     extractStateReview,
		req:       req,
		srcSize:   srcSize,
		staging:   staging,
		final:     final,
	}, nil
}

// supersede bumps gen and cancels any in-flight per-op context. Called from
// every transition that starts a new restic call and from the back-to-browse
// path so a late message from the canceled run is dropped by gen check.
func (m *extractModel) supersede() {
	m.gen++
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
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

// extractBackToBrowseMsg is dispatched by the sub-model when it wants the root
// model to leave extractView. The root model handles the switch back to the
// originating view (browse or detail, per extractReturn) and drops the whole
// sub-model (zeroing every transient path field). We use a message rather than
// a direct mutation so the sub-model stays self-contained.
type extractBackToBrowseMsg struct {
	notice string
}

// startRun kicks off the live extract. Returns a tea.Batch of the worker Cmd
// and the progress-pump Cmd. Mirrors browse.beginIndex.
func (m *extractModel) startRun() tea.Cmd {
	m.supersede()
	runCtx, cancel := context.WithCancel(m.parentCtx)
	m.cancel = cancel
	gen := m.gen
	req := m.req
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

// waitForExtractProgress is the progress-channel pump. Mirrors
// waitForIndexProgress / waitForDiffProgress.
func waitForExtractProgress(gen int, progress <-chan app.ExtractProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-progress
		if !ok {
			return nil
		}
		return extractProgressMsg{gen: gen, progress: p}
	}
}

// ensureFilepicker constructs the embedded filepicker on first use. The
// current directory falls back to the user's home if the configured TargetRoot
// is empty or unreadable.
func (m *extractModel) ensureFilepicker() tea.Cmd {
	if m.filepickerInit {
		return nil
	}
	m.filepickerInit = true
	fp := filepicker.New()
	fp.DirAllowed = true
	fp.FileAllowed = false
	// Show dotfiles so target roots under hidden directories (e.g. ~/.local,
	// an automounted ~/.cache path) are reachable; only directories are
	// selectable, files render disabled.
	fp.ShowHidden = true
	fp.ShowPermissions = true
	fp.ShowSize = true
	// AutoHeight is off (the picker is embedded under our own header/footer), so
	// we size it ourselves here and again on every WindowSizeMsg.
	fp.AutoHeight = false
	fp.SetHeight(extractFilePickerHeight(m.height))
	dir := m.cfg.TargetRoot
	if dir == "" || !filepath.IsAbs(dir) {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = "/"
		}
	}
	fp.CurrentDirectory = dir
	m.filepicker = fp
	return m.filepicker.Init()
}

// extractFilePickerHeight sizes the embedded picker's scroll viewport from the
// terminal height, reserving rows for our header / hint / footer scaffolding.
// Falls back to a fixed window before the first WindowSizeMsg lands.
func extractFilePickerHeight(termHeight int) int {
	if h := termHeight - 8; h > 1 {
		return h
	}
	return 10
}

// setHeight syncs the terminal height into the sub-model — and, when the
// filepicker has been built, resizes its viewport (AutoHeight is off, so we own
// its sizing). The single place size propagation lives: the root calls it on
// every WindowSizeMsg and before key dispatch, so a lazily-built picker sizes
// from the live terminal.
func (m *extractModel) setHeight(h int) {
	m.height = h
	if m.filepickerInit {
		m.filepicker.SetHeight(extractFilePickerHeight(h))
	}
}

// updateFilePicker forwards a non-key message to the embedded filepicker. The
// bubbles filepicker is fully async: Init and every navigation return a command
// that produces an (unexported) readDirMsg, and that message is the only thing
// that populates the directory list. The root Model routes those messages here
// from its Update default branch while the overlay is open; without it the
// picker would render forever empty.
func (m extractModel) updateFilePicker(msg tea.Msg) (extractModel, tea.Cmd) {
	var cmd tea.Cmd
	m.filepicker, cmd = m.filepicker.Update(msg)
	return m, cmd
}

// applyRunDone installs the result of a live run.
func (m *extractModel) applyRunDone(msg extractRunDoneMsg) {
	if msg.gen != m.gen {
		return
	}
	m.result = msg.result
	if msg.err != nil {
		m.err = msg.err
		if isExtractCancelErr(msg.err) {
			m.state = extractStateCanceled
		} else {
			m.state = extractStateError
		}
		// Probe staging fate once at the transition so the terminal renders read
		// the cached value; handleTerminalKey re-probes per keypress.
		m.stagingExists = m.stagingPending()
		return
	}
	m.err = nil
	m.state = extractStateSuccess
}

// applyProgress records a progress sample and re-arms the wait pump. Each field
// is merged monotonically (max-of-seen) rather than via a wholesale replace, so a
// non-monotonic restic status event — e.g. a run of zero-byte files that leaves
// bytes_done flat while files_done climbs — can never momentarily regress a
// counter. The rate sampler only advances when bytes actually grew.
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

// applyDeleteStagingDone closes the modal regardless of err (the close itself is
// the returned back-to-browse Cmd, so every exit funnels through the single
// extractBackToBrowseMsg path in the root model); on failure the error — already
// path-free, app.DeleteExtractStaging owns that contract — rides along as the
// notice surfaced on return to browse.
func (m *extractModel) applyDeleteStagingDone(msg extractDeleteStagingDoneMsg) tea.Cmd {
	if msg.gen != m.gen {
		return nil
	}
	if msg.err != nil {
		m.noticeAfterClose = firstLine(msg.err.Error())
	}
	return returnExtract(m.noticeAfterClose)
}

// back is the single per-state step-back implementation: cancel when running,
// close the filepicker, leave the modal from every other state. The root routes
// `q` here directly and every handler's esc (keys.Back) arm delegates here, so
// the two paths can never drift.
func (m extractModel) back() (extractModel, tea.Cmd, bool) {
	switch m.state {
	case extractStateRunning:
		// Cancel the in-flight extract via the per-op cancel; the worker
		// goroutine will deliver extractRunDoneMsg with context.Canceled and
		// applyRunDone moves us to extractStateCanceled.
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
		// Supersede NOW, not when the root processes extractBackToBrowseMsg:
		// a gen-tagged result already in flight (e.g. the sudo probe behind a
		// busy review) could otherwise land first and commit a run the user
		// just backed out of. The root's supersede on the back message is then
		// a harmless second bump.
		m.supersede()
		return m, returnExtract(m.noticeAfterClose), true
	}
	return m, nil, false
}

// handleKey routes a single key press. Returns the new model, an optional Cmd,
// and a bool indicating whether the sub-model wants to leave the modal entirely
// (in which case the root handles the view switch and drops the sub-model).
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
	// One debounce for the whole screen: while the sudo probe / interactive
	// auth is in flight, only esc still acts.
	if m.sudoBusy && !key.Matches(msg, keys.Back) {
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
		// Toggle the privileged (sudo) restore: restic only applies snapshot
		// ownership when it runs as root. Whether sudo is actually ready is
		// checked on commit, not here.
		m.req.Privileged = !m.req.Privileged
		m.reviewNotice = ""
		return m, nil, false
	case key.Matches(msg, keys.Enter):
		m.reviewNotice = ""
		if m.req.Privileged {
			// Privileged commit: confirm cached sudo auth before dispatching; the
			// run starts on the probe (or interactive-auth) message. The probe is
			// a new async step, so it gets its own generation like every other.
			m.supersede()
			m.sudoBusy = true
			return m, m.sudoProbeCmd(), false
		}
		// Commit straight to the live extract — a single Extract for both file and
		// directory sources. There is no dry-run preview step.
		cmd := m.commitRun()
		return m, cmd, false
	}
	return m, nil, false
}

// commitRun is the single dispatch point for committing the review screen: it
// clears the sudo gate, starts the live extract, and moves to running. All
// three commit paths (plain enter, clean sudo probe, interactive auth
// success) funnel through it.
func (m *extractModel) commitRun() tea.Cmd {
	m.sudoBusy = false
	cmd := m.startRun()
	m.state = extractStateRunning
	return cmd
}

// sudoProbeCmd asks the app layer whether a privileged helper launch would
// prompt. Gen-tagged like every other async extract step.
func (m *extractModel) sudoProbeCmd() tea.Cmd {
	gen := m.gen
	drv := m.drv
	ctx := m.parentCtx
	return func() tea.Msg {
		return extractSudoProbeMsg{gen: gen, err: drv.PrivilegedExtractProbe(ctx)}
	}
}

// applySudoProbe resolves the probe: ready → start the run; unavailable →
// surface a review notice; needs auth → suspend the TUI for an interactive
// `sudo -v` (the helper itself always runs with -n against the then-warm
// credential cache, so it can never hang on a hidden prompt).
func (m *extractModel) applySudoProbe(msg extractSudoProbeMsg) tea.Cmd {
	if msg.gen != m.gen || m.state != extractStateReview || !m.sudoBusy {
		return nil
	}
	if msg.err == nil {
		return m.commitRun()
	}
	authCmd := m.drv.PrivilegedAuthCommand()
	if errors.Is(msg.err, app.ErrPrivilegedExtractUnavailable) || authCmd == nil {
		m.sudoBusy = false
		m.reviewNotice = "privileged extract not available"
		return nil
	}
	gen := m.gen
	return tea.ExecProcess(authCmd, func(err error) tea.Msg {
		return extractSudoAuthMsg{gen: gen, err: err}
	})
}

// applySudoAuth resumes after the interactive sudo -v: success starts the run,
// failure lands back on review with a path-free notice.
func (m *extractModel) applySudoAuth(msg extractSudoAuthMsg) tea.Cmd {
	if msg.gen != m.gen || m.state != extractStateReview || !m.sudoBusy {
		return nil
	}
	if msg.err != nil {
		m.sudoBusy = false
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
	case key.Matches(msg, keys.Back), key.Matches(msg, keys.Enter):
		return m.back()
	case key.Matches(msg, keys.Shell):
		// Drop the user into a credential-free shell rooted at the extracted
		// directory. The session-prep error path mirrors openShellCmd: surface it
		// via shellExitedMsg (path-free by construction) rather than swallowing it.
		// The success screen stays up; tea.ExecProcess returns to it on shell exit.
		sess, err := m.drv.LocalShellSession(m.result.FinalDir)
		if err != nil {
			return m, func() tea.Msg { return shellExitedMsg{err: err} }, false
		}
		return m, shellCmdFromSession(sess), false
	}
	return m, nil, false
}

// handleTerminalKey routes the canceled / error screens. When the result
// reports StagingCreated=true and the staging dir still exists on disk, the
// user is given the keep-or-delete prompt. Otherwise enter/esc returns to
// browse directly. The staging probe is re-run before any key this state acts
// on (a dir the user deleted out-of-band must stop offering the prompt) and
// cached on stagingExists for the render path; ignored keys skip it, since the
// underlying Lstat can block against an automounted target root.
func (m extractModel) handleTerminalKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if !key.Matches(msg, keys.Delete) && !key.Matches(msg, keys.Keep) &&
		!key.Matches(msg, keys.Enter) && !key.Matches(msg, keys.Back) &&
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
			key.Matches(msg, keys.Enter),
			key.Matches(msg, keys.Back):
			return m.back()
		}
		return m, nil, false
	}
	switch {
	case isExtractRefusal(m.err) && key.Matches(msg, keys.Target):
		// The refusal hint tells the user to press t to choose another target; honor
		// it here so the key is not a no-op. There is no staging to orphan in this
		// branch (the staging-exists case is handled above with keep/delete), so the
		// failed run's transient outcome is cleared and we reopen the picker — a
		// selection re-plans and lands back on review, ready to retry.
		m.err = nil
		m.result = app.ExtractResult{}
		m.state = extractStateFilePicker
		m.filepickerErr = ""
		cmd := m.ensureFilepicker()
		return m, cmd, false
	case key.Matches(msg, keys.Enter), key.Matches(msg, keys.Back):
		return m.back()
	}
	return m, nil, false
}

// handleKeepDeleteKey covers the brief window between dispatching the staging
// delete and its done message. esc takes the user back; the actual close
// happens on extractDeleteStagingDoneMsg.
func (m extractModel) handleKeepDeleteKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) {
		// Abort waiting and just return; the in-flight delete still completes
		// in its goroutine but its done-msg is discarded by the gen check
		// after we supersede on the way out.
		return m.back()
	}
	return m, nil, false
}

// handleFilePickerKey forwards keys to the embedded filepicker, polling
// DidSelectFile each tick. On a directory selection we re-plan with that root
// as a per-op override and refuse if the derived staging / final already exist.
func (m extractModel) handleFilePickerKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) {
		// esc inside the picker closes the overlay and returns to review.
		// The picker's own Back binding also matches esc, so we'd otherwise
		// ascend a directory level before the overlay closes; catch esc here
		// first.
		return m.back()
	}
	var cmd tea.Cmd
	m.filepicker, cmd = m.filepicker.Update(msg)
	if didSelect, fppath := m.filepicker.DidSelectFile(msg); didSelect {
		if newReq, staging, final, perr := planExtractOverride(m.cfg, m.req, fppath); perr == nil {
			m.req = newReq
			m.staging = staging
			m.final = final
			m.state = extractStateReview
			m.filepickerErr = ""
		} else {
			m.filepickerErr = firstLine(perr.Error())
		}
	} else if didSelect, _ := m.filepicker.DidSelectDisabledFile(msg); didSelect {
		m.filepickerErr = "select a directory"
	}
	return m, cmd, false
}

// planExtractOverride re-plans staging / final under a chosen target root and
// rejects if either child already exists — the same occupancy rule and path-free
// sentinels App.Extract enforces, via app.FreshTargetCheck, so the picker's
// refusal can never drift from the run-time one (and isExtractRefusal matches
// it). Returns the mutated request alongside the new paths.
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

// deleteStagingCmd schedules the user-confirmed delete of the staging dir via
// app.DeleteExtractStaging (which owns the path-free error contract). The path
// is the exact one App.Extract reported as created by this run; we re-verify it
// equals m.staging (defense in depth) before removing.
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

// stagingPending reports whether this run created a staging dir that is still
// on disk — the condition for offering the keep-or-delete prompt. Probed at
// state transitions and terminal-state keypresses, never from the render path
// (which reads the cached stagingExists field instead).
func (m extractModel) stagingPending() bool {
	return m.result.StagingCreated && stagingDirExists(m.result.StagingDir)
}

// stagingDirExists is a path-presence check (false for the empty path, so an
// unset StagingDir never reads the filesystem).
func stagingDirExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// isExtractCancelErr classifies whether the error came from a per-op
// cancel / timeout. It checks both context errors directly and the wrapped
// form App.Extract produces ("extract: <context.Canceled>").
func isExtractCancelErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isExtractRefusal reports whether err is an occupied-target/staging refusal — the
// only terminal error whose remedy is to retarget (t) or clear the occupant. It
// gates the actionable refusal hint, the terminal-state `t` affordance, and the
// terminal footer so the three stay in lockstep: the hint never names a key the
// handler ignores.
func isExtractRefusal(err error) bool {
	return errors.Is(err, app.ErrExtractFinalExists) || errors.Is(err, app.ErrExtractStagingExists)
}

// returnExtract dispatches a back-to-browse message carrying an optional
// notice. We use a Cmd so the sub-model stays self-contained and the root
// Model owns the actual view switch + clear.
func returnExtract(notice string) tea.Cmd {
	return func() tea.Msg {
		return extractBackToBrowseMsg{notice: notice}
	}
}

// extractRequestFromBrowseEntry translates a browse selection into a fully
// explicit ExtractRequest the app layer accepts. The browse `e` keypress uses it
// before opening the modal. Symlinks / devices / fifos / sockets are rejected
// with ErrExtractUnsupportedType; the app layer's defense-in-depth gate catches a
// mode / type mismatch a second time.
func extractRequestFromBrowseEntry(repo, snapID string, entry model.BrowseEntry) (app.ExtractRequest, error) {
	if len(snapID) < 8 {
		return app.ExtractRequest{}, errors.New("snapshot id too short")
	}
	snapShort := snapID[:8]
	source := model.CleanBrowsePath(entry.Path)
	var mode app.ExtractMode
	var wasFile bool
	switch {
	case entry.Type == "file":
		// A regular file restores via restic restore --include and lands at its true
		// mirror path under the per-snapshot directory.
		mode = app.ExtractFile
		wasFile = true
	case entry.Type == "dir" || entry.IsDir:
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

// extractRequestFromFindVersion translates a find-versions row into a file
// ExtractRequest against one of the version's occurrence snapshots. The find
// table lists versions of the regular file browse launched `v` from, so the
// request is the same file shape browse's `e` builds — routed through
// extractRequestFromBrowseEntry with a synthetic file entry so the path and
// slug rules can never drift between the two entry points.
func extractRequestFromFindVersion(repo, snapID, p string) (app.ExtractRequest, error) {
	return extractRequestFromBrowseEntry(repo, snapID, model.BrowseEntry{Path: p, Type: "file"})
}

// extractRequestFromSnapshot translates a detail-view snapshot selection into
// the whole-snapshot ExtractRequest (Source "/"). The detail `e` keypress uses
// it before opening the modal. PlanExtractPaths requires SourceName ==
// SnapshotShort for the root source and collapses the final path to the
// snapshot dir itself, so the published tree lands at
// <target_root>/<repo>/<short>/.
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

// shortHelp produces the modal's per-state footer bindings, rendered by
// footerView through the same bubbles help model as every other view (so the
// styling and separator can never drift). Kept here next to handleKey so the
// advertised keys and the handled keys stay in sync.
func (m extractModel) shortHelp(keys keyMap) []key.Binding {
	switch m.state {
	case extractStateReview:
		// Mirroring is inherently nested — one review footer for both file and
		// directory sources, no layout toggle.
		return []key.Binding{helpAs(keys.Enter, "extract"), keys.Target, keys.Priv, keys.Back}
	case extractStateRunning:
		return []key.Binding{helpAs(keys.Back, "cancel")}
	case extractStateSuccess:
		// "back" rather than "back to browse": the modal launches from browse
		// and detail alike, and the sub-model doesn't know its origin.
		return []key.Binding{helpAs(keys.Shell, "shell here"), helpAs(keys.Enter, "back")}
	case extractStateCanceled, extractStateError:
		if m.stagingExists {
			return []key.Binding{keys.Keep, keys.Delete}
		}
		if isExtractRefusal(m.err) {
			// Advertise the retarget affordance the hint points to (handleTerminalKey
			// honors t in this branch).
			return []key.Binding{keys.Target, helpAs(keys.Enter, "back")}
		}
		return []key.Binding{helpAs(keys.Enter, "back")}
	case extractStateFilePicker:
		return []key.Binding{keys.Back}
	case extractStateKeepDelete:
		return []key.Binding{helpAs(keys.Back, "back")}
	}
	return nil
}
