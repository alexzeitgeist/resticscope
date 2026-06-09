package tui

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
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
// lifetime of the modal — clearExtract zeros every transient field on every exit
// path so no path data lingers in the model (non-negotiable #1, same discipline
// as browse / find-versions / snapshot-diff).
//
// Browse wires the `e` key to construct this sub-model from the selected
// BrowseEntry via extractRequestFromBrowseEntry. The success-view `s` action
// opens a credential-free local shell rooted at the extracted directory via
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

// extractRateWindow is the minimum sample span for the displayed recent
// extract throughput rate, matching the browse indexer's window so both feel
// the same to the user.
const extractRateWindow = 2 * time.Second

// extractDriver is the small consumer-side interface the sub-model needs from
// the app layer (engineering rule 3: tiny, defined where it is used). Satisfied
// by *app.App; the test double in extract_test.go implements it directly.
// LocalShellSession backs the success-view `s` action: a credential-free shell
// rooted at the extracted directory.
type extractDriver interface {
	Extract(ctx context.Context, req app.ExtractRequest, onProgress func(app.ExtractProgress)) (app.ExtractResult, error)
	LocalShellSession(dir string) (*app.ShellSession, error)
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
	// shown on the review "Type" line. Zeroed by clearTransient with everything
	// else so no size derived from a path lingers after leaving the modal.
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

	// progress is the latest sampled snapshot during running. rateBaseN /
	// rateBaseAt / rate compute the sampled MiB/s for the running view, using
	// the same window-sampled approach as the browse indexer (copy of the
	// pattern per the step-05 plan; TODO: consolidate when both views need it).
	progress   app.ExtractProgress
	rateBaseN  int64
	rateBaseAt time.Time
	rate       float64

	// filepicker is constructed lazily on first entry to extractStateFilePicker
	// so the embedded model only reads disk when the user opens it.
	filepicker     filepicker.Model
	filepickerInit bool
	filepickerErr  string

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

// clearTransient zeros every path-bearing / per-op field so the sub-model can
// be safely dropped on leaving the modal. Called on every back-to-browse exit
// regardless of state (non-negotiable #1: no filenames linger).
func (m *extractModel) clearTransient() {
	m.supersede()
	m.state = extractStateReview
	m.req = app.ExtractRequest{}
	m.srcSize = 0
	m.staging = ""
	m.final = ""
	m.result = app.ExtractResult{}
	m.err = nil
	m.progress = app.ExtractProgress{}
	m.progressCh = nil
	m.rateBaseN = 0
	m.rateBaseAt = time.Time{}
	m.rate = 0
	m.filepicker = filepicker.Model{}
	m.filepickerInit = false
	m.filepickerErr = ""
	m.noticeAfterClose = ""
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

// extractBackToBrowseMsg is dispatched by the sub-model when it wants the root
// model to leave extractView. The root model handles the view switch and the
// clearTransient call. We use a message rather than a direct mutation so the
// sub-model stays self-contained.
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
	m.rateBaseN = 0
	m.rateBaseAt = time.Time{}
	m.rate = 0
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
		m.updateRate(p.BytesDone, time.Now())
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
	// seconds_remaining is an ETA that legitimately counts down, so accept the
	// latest non-zero value rather than the max.
	if p.SecondsRemaining > 0 {
		m.progress.SecondsRemaining = p.SecondsRemaining
	}
	return waitForExtractProgress(msg.gen, m.progressCh)
}

func (m *extractModel) updateRate(n int64, now time.Time) {
	if m.rateBaseAt.IsZero() {
		m.rateBaseN = n
		m.rateBaseAt = now
		return
	}
	elapsed := now.Sub(m.rateBaseAt)
	if elapsed < extractRateWindow || n <= m.rateBaseN {
		return
	}
	m.rate = float64(n-m.rateBaseN) / elapsed.Seconds()
	m.rateBaseN = n
	m.rateBaseAt = now
}

// applyDeleteStagingDone closes the keep-delete overlay regardless of err; on
// failure we attach a notice surfaced on return to browse.
func (m *extractModel) applyDeleteStagingDone(msg extractDeleteStagingDoneMsg) {
	if msg.gen != m.gen {
		return
	}
	if msg.err != nil {
		// Path-free per the privacy contract: os.RemoveAll's error embeds the
		// staging path, which would otherwise land in the persistent status line
		// on return to browse. Surface only that it failed.
		m.noticeAfterClose = "extract: could not delete staging directory"
	}
}

// back is the routing entry-point for `q` from the root. It mirrors the same
// per-state behavior the in-modal esc binding triggers (cancel running, close
// the filepicker, accept defaults on terminal states, etc.) without having to
// synthesize a KeyPressMsg.
func (m extractModel) back(keys keyMap) (extractModel, tea.Cmd, bool) {
	switch m.state {
	case extractStateRunning:
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
		return m, returnExtract(m.noticeAfterClose), true
	}
	return m, nil, false
}

// handleKey routes a single key press. Returns the new model, an optional Cmd,
// and a bool indicating whether the sub-model wants to leave the modal entirely
// (in which case the root handles the view switch + clearTransient).
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
	switch {
	case key.Matches(msg, keys.Back), key.Matches(msg, keys.Quit):
		return m, returnExtract(""), true
	case key.Matches(msg, keys.Target):
		m.state = extractStateFilePicker
		m.filepickerErr = ""
		cmd := m.ensureFilepicker()
		return m, cmd, false
	case key.Matches(msg, keys.Enter):
		// Commit straight to the live extract — a single Extract for both file and
		// directory sources. There is no dry-run preview step.
		cmd := m.startRun()
		m.state = extractStateRunning
		return m, cmd, false
	}
	return m, nil, false
}

func (m extractModel) handleRunningKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) || key.Matches(msg, keys.Quit) {
		// Cancel the in-flight extract via the per-op cancel; the worker
		// goroutine will deliver extractRunDoneMsg with context.Canceled and
		// applyRunDone moves us to extractStateCanceled.
		if m.cancel != nil {
			m.cancel()
		}
		return m, nil, false
	}
	return m, nil, false
}

func (m extractModel) handleSuccessKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Back), key.Matches(msg, keys.Quit), key.Matches(msg, keys.Enter):
		return m, returnExtract(""), true
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
// browse directly.
func (m extractModel) handleTerminalKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if m.result.StagingCreated && m.result.StagingDir != "" && stagingDirExists(m.result.StagingDir) {
		switch {
		case key.Matches(msg, keys.Delete):
			m.state = extractStateKeepDelete
			return m, m.deleteStagingCmd(), false
		case key.Matches(msg, keys.Keep),
			key.Matches(msg, keys.Enter),
			key.Matches(msg, keys.Back),
			key.Matches(msg, keys.Quit):
			return m, returnExtract(""), true
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
	case key.Matches(msg, keys.Enter), key.Matches(msg, keys.Back), key.Matches(msg, keys.Quit):
		return m, returnExtract(""), true
	}
	return m, nil, false
}

// handleKeepDeleteKey covers the brief window between dispatching the staging
// delete and its done message. esc/enter take the user back; the actual close
// happens on extractDeleteStagingDoneMsg.
func (m extractModel) handleKeepDeleteKey(keys keyMap, msg tea.KeyPressMsg) (extractModel, tea.Cmd, bool) {
	if key.Matches(msg, keys.Back) || key.Matches(msg, keys.Quit) {
		// Abort waiting and just return; the in-flight delete still completes
		// in its goroutine but its done-msg is discarded by the gen check
		// after we supersede on the way out.
		return m, returnExtract(m.noticeAfterClose), true
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
		m.state = extractStateReview
		m.filepickerErr = ""
		return m, nil, false
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
// rejects if either child already exists. Returns the mutated request alongside
// the new paths.
func planExtractOverride(cfg config.Extract, base app.ExtractRequest, root string) (app.ExtractRequest, string, string, error) {
	req := base
	req.TargetRoot = root
	staging, final, err := app.PlanExtractPaths(cfg, req)
	if err != nil {
		return base, "", "", err
	}
	if _, lerr := os.Lstat(staging); lerr == nil {
		return base, "", "", errors.New("staging directory already exists")
	}
	if _, lerr := os.Lstat(final); lerr == nil {
		return base, "", "", errors.New("target already exists")
	}
	return req, staging, final, nil
}

// deleteStagingCmd schedules the user-confirmed RemoveAll of the staging dir.
// The path is the exact one App.Extract reported as created by this run; we
// re-verify it equals m.staging (defense in depth) before removing.
func (m extractModel) deleteStagingCmd() tea.Cmd {
	gen := m.gen
	staging := m.result.StagingDir
	expected := m.staging
	return func() tea.Msg {
		if staging == "" || staging != expected {
			return extractDeleteStagingDoneMsg{gen: gen, err: errors.New("staging path mismatch; refusing delete")}
		}
		return extractDeleteStagingDoneMsg{gen: gen, err: os.RemoveAll(staging)}
	}
}

// stagingDirExists is a path-presence check used by the terminal-state handler
// to decide whether to offer the keep-or-delete prompt at all (a staging dir
// the user deleted out-of-band must not show the prompt).
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

// extractFootRender produces the modal's footer help line for the current
// state. Kept here next to handleKey so the two stay in sync.
func (m extractModel) helpLine(keys keyMap, st styles) string {
	switch m.state {
	case extractStateReview:
		// Mirroring is inherently nested — one review footer for both file and
		// directory sources, no layout toggle.
		return joinHelp(st,
			keyHelp(st, keys.Enter, "extract"),
			keyHelp(st, keys.Target, "target"),
			keyHelp(st, keys.Back, "back"),
		)
	case extractStateRunning:
		return joinHelp(st, keyHelp(st, keys.Back, "cancel"))
	case extractStateSuccess:
		return joinHelp(st,
			keyHelp(st, keys.Shell, "shell here"),
			keyHelp(st, keys.Enter, "back to browse"),
		)
	case extractStateCanceled, extractStateError:
		if m.result.StagingCreated && stagingDirExists(m.result.StagingDir) {
			return joinHelp(st,
				keyHelp(st, keys.Keep, "keep"),
				keyHelp(st, keys.Delete, "delete"),
			)
		}
		if isExtractRefusal(m.err) {
			// Advertise the retarget affordance the hint points to (handleTerminalKey
			// honors t in this branch).
			return joinHelp(st,
				keyHelp(st, keys.Target, "target"),
				keyHelp(st, keys.Enter, "back to browse"),
			)
		}
		return joinHelp(st, keyHelp(st, keys.Enter, "back to browse"))
	case extractStateFilePicker:
		return joinHelp(st, keyHelp(st, keys.Back, "back"))
	case extractStateKeepDelete:
		return joinHelp(st, keyHelp(st, keys.Back, "back to browse"))
	}
	return ""
}

// keyHelp renders one "<key> <desc>" hint with the key colored like every other
// view's footer. It uses the binding's help label (b.Help().Key), not its raw
// bound keys: Back is bound to esc but advertises "q" (see keys.go), so this is
// what shows the canonical "q back" the rest of the app already uses.
func keyHelp(st styles, b key.Binding, desc string) string {
	return st.key.Render(b.Help().Key) + " " + st.meta.Render(desc)
}

func joinHelp(st styles, parts ...string) string {
	return strings.Join(parts, st.dim.Render(" · "))
}
