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
	// SubtreeCounts backs the review screen's Contains row for directory
	// sources: contained file/dir counts from the browse index, known=false
	// when the snapshot has no committed index.
	SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error)
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

	// ranTargetRoot is req.TargetRoot as of the last dispatched run — "" when no
	// run started or the run used the config-default root. The root model copies
	// it into its session-scoped extractTargetMemo on modal close, so a target
	// is remembered only once an extract actually ran against it (cancelled or
	// failed runs count); a picker selection the user backed out of without
	// running is dropped with the rest of the sub-model.
	ranTargetRoot string

	// queue holds the not-yet-run sides of a multi-request (diff) extract, in
	// run order. applyRunDone advances through it: each clean completion either
	// starts the next side or lands on success once the queue drains. Empty for
	// a plain extract.
	queue []app.ExtractRequest

	// published accumulates the cleanly published sides of a diff extract: the
	// done screen reports each side, and the terminal screens note what already
	// landed before a failure or cancel. Always empty for a plain extract,
	// which reads m.result directly.
	published []extractSideResult

	// diff carries the diff-extract flavor: the directional pair, per-side
	// change counts, and the container display path. nil for a plain extract.
	// Held only for the modal's lifetime like every other path-bearing field.
	diff *extractDiffMeta

	// srcSize is the source node's size from the originating BrowseEntry (a
	// directory's recursive subtree size, a file's byte length). Display-only:
	// shown on the review "Type" line; dropped with the rest of the sub-model on
	// exit so no size derived from a path lingers after leaving the modal.
	srcSize int64

	// srcFiles / srcDirs are a directory source's contained file/dir counts,
	// loaded async from the browse index after the review opens; srcCountsKnown
	// gates the review Contains row (file sources, unindexed snapshots, and
	// failed lookups leave it false). Display-only like srcSize, and dropped
	// with the sub-model on exit.
	srcFiles, srcDirs int
	srcCountsKnown    bool

	// targetBusy notes that staging or final was already occupied when the
	// review opened — the same FreshTargetCheck enter will enforce, surfaced
	// early as an advisory note so the collision isn't a surprise refusal
	// screen. Advisory only: the run-time check stays authoritative, and a
	// successful retarget through the filepicker clears it (planExtractOverride
	// refuses occupied roots).
	targetBusy bool

	// targetFree / targetFreeKnown carry the review-time free-space probe of
	// the target filesystem (ExtractFreeSpace at the planned staging path); the
	// review warns when the source's known size exceeds it. Advisory like
	// targetBusy — never blocks enter — and re-probed on retarget.
	targetFree      int64
	targetFreeKnown bool

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
	// pickerStyles is resolved from the configured theme by the constructors
	// (which hold the App; ensureFilepicker doesn't), ready for that first use.
	filepicker     filepicker.Model
	filepickerInit bool
	filepickerErr  string
	pickerStyles   filepicker.Styles

	// pickerModeW is the widest FileMode string (in cells) across the picker's
	// whole current directory, scanned asynchronously (pickerModeWidthCmd)
	// whenever syncPickerModeWidth notices the picker navigated.
	// alignFilePickerModes pads to it so the columns don't shift as wide-mode
	// rows (e.g. a sticky dir's "dtrwxrwxrwx") scroll in and out of the
	// viewport — the picker keeps its file list unexported, so the directory is
	// re-read to see past the visible window. pickerModeDir is the directory
	// the last scan was issued for (the recompute edge detector).
	pickerModeW   int
	pickerModeDir string

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

// seedTargetMemo applies the session's remembered extract target root to a
// freshly-built request, so repeat extracts land where the user last actually
// ran one without re-picking through the filepicker each time. The memo is
// only ever a root a run dispatched against (see extractTargetMemo), and a
// request that already carries an explicit override is left alone.
func (m Model) seedTargetMemo(req app.ExtractRequest) app.ExtractRequest {
	if req.TargetRoot == "" {
		req.TargetRoot = m.extractTargetMemo
	}
	return req
}

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
		// A few stats, same order of cost as a filepicker selection pays in
		// planExtractOverride.
		targetBusy:      isExtractRefusal(app.FreshTargetCheck(staging, final)),
		targetFree:      free,
		targetFreeKnown: freeKnown,
	}, nil
}

// extractDiffMeta is the diff-extract flavor of the modal. firstShort /
// secondShort are the pair in display order (left / right of the diff view's
// arrow); firstCount / secondCount are the post-filter changed-path counts per
// side (0 marks a side with nothing to extract, which is skipped — no request
// is queued for it); containerDir is the published pair container the review /
// done screens show and shell-here lands in.
type extractDiffMeta struct {
	firstShort, secondShort string
	filters                 model.ModifierKind
	sourceIsDir             bool
	firstCount, secondCount int
	containerDir            string
}

// extractSideResult pairs one published side's result with its snapshot short
// id for the per-side lines on the done / terminal screens.
type extractSideResult struct {
	short  string
	result app.ExtractResult
}

// newExtractDiffModel constructs the sub-model for a diff extract: one request
// per side with content (in display order), sharing one pair container. Every
// side is planned up front so an invalid request refuses the open instead of
// surfacing mid-run, and the review preflights both sides' targets — the busy
// note shows when either side's staging or final is occupied.
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
	// Both sides publish under one container, so the first side's staging
	// answers for the shared filesystem (same root as a plain extract's probe).
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
		targetBusy:      busy,
		targetFree:      free,
		targetFreeKnown: freeKnown,
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

// extractCountsMsg delivers the review screen's directory-contents counts. A
// lookup that failed or found no committed index arrives with known=false and
// is dropped — the counts are advisory display detail, never a gate.
type extractCountsMsg struct {
	gen         int
	files, dirs int
	known       bool
}

// countsCmd queries the browse index for a directory source's contained
// file/dir counts. nil for file sources (the count is trivially one), for a
// zero sub-model (a failed open leaves m.extract empty, with a nil drv), and
// for a diff extract (its review reports per-side changed-path counts from the
// diff data instead — the index's whole-subtree counts would be wrong there).
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

// applyCounts fills the review Contains row from a counts lookup. Late
// messages from a superseded generation are dropped like every other extract
// message.
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
// current directory starts at the request's target-root override when set,
// then the configured TargetRoot, falling back to the user's home.
func (m *extractModel) ensureFilepicker() tea.Cmd {
	if m.filepickerInit {
		return nil
	}
	m.filepickerInit = true
	fp := filepicker.New()
	fp.Styles = m.pickerStyles
	// The same accent gutter glyph every list in the app marks its cursor row
	// with (browse, detail, snapshot diff).
	fp.Cursor = "▎"
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
	// Start at the effective target root: a request-level override (the session
	// memo seeded at construction) wins over the configured default, so a
	// re-pick begins where the last run landed.
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

// syncPickerModeWidth notices a picker directory change and returns the
// command that scans the new directory for pickerModeW — async like the
// picker's own readDir, so a large or slow (automounted) directory never
// stalls the update loop. nil while the directory is unchanged. Until the
// result lands the width is reset to 0 and alignFilePickerModes falls back to
// the on-screen max (the pre-scan behavior).
func (m *extractModel) syncPickerModeWidth() tea.Cmd {
	dir := m.filepicker.CurrentDirectory
	if dir == m.pickerModeDir {
		return nil
	}
	m.pickerModeDir = dir
	m.pickerModeW = 0
	return pickerModeWidthCmd(dir)
}

// extractPickerModeWidthMsg carries one pickerModeWidthCmd result. dir is the
// directory the scan ran over — the staleness guard: updateFilePicker applies
// w only while the picker is still in that directory.
type extractPickerModeWidthMsg struct {
	dir string
	w   int
}

// pickerModeWidthCmd scans dir for the widest FileMode string, with the same
// lstat-level os.ReadDir + Info the picker itself renders from, so the width
// matches every row the picker can ever scroll to (ShowHidden is on, so the
// picker filters nothing out). On a read error w stays 0 and the on-screen
// fallback persists for this directory.
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
			// Mode strings are ASCII, so byte length is the cell count.
			if n := len(info.Mode().String()); n > out.w {
				out.w = n
			}
		}
		return out
	}
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
	if wm, ok := msg.(extractPickerModeWidthMsg); ok {
		// Stale-guard: a scan that raced a navigation reports a directory the
		// picker has already left; its width belongs to the wrong listing.
		if wm.dir == m.filepicker.CurrentDirectory {
			m.pickerModeW = wm.w
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.filepicker, cmd = m.filepicker.Update(msg)
	return m, tea.Batch(cmd, m.syncPickerModeWidth())
}

// applyRunDone installs the result of a live run. On a clean completion with
// queued sides remaining (a diff extract), the returned Cmd starts the next
// side instead of landing on success.
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
		// Probe staging fate once at the transition so the terminal renders read
		// the cached value; handleTerminalKey re-probes per keypress.
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

// advanceQueue starts the next queued side of a diff extract. The side's
// request inherits the review-time toggles from the side that just ran (the
// privileged flag and any filepicker retarget apply to the operation as a
// whole), and its staging / final re-derive from the inherited root. A plan
// failure here is unreachable after the constructor planned every side, but
// degrades to the normal error screen rather than a panic.
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
	case key.Matches(msg, keys.Back):
		return m.back()
	case key.Matches(msg, keys.Shell):
		// Drop the user into a credential-free shell rooted at the extracted
		// directory — for a diff extract that is the pair container, where both
		// snapshot roots are visible (and diff -r away). The session-prep error
		// path mirrors openShellCmd: surface it via shellExitedMsg (path-free by
		// construction) rather than swallowing it. The success screen stays up;
		// tea.ExecProcess returns to it on shell exit.
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

// handleTerminalKey routes the canceled / error screens. When the result
// reports StagingCreated=true and the staging dir still exists on disk, the
// user is given the keep-or-delete prompt. Otherwise esc returns to
// browse directly. The staging probe is re-run before any key this state acts
// on (a dir the user deleted out-of-band must stop offering the prompt) and
// cached on stagingExists for the render path; ignored keys skip it, since the
// underlying Lstat can block against an automounted target root.
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
		// The refusal hint tells the user to press t to choose another target; honor
		// it here so the key is not a no-op. There is no staging to orphan in this
		// branch (the staging-exists case is handled above with keep/delete), so the
		// failed run's transient outcome is cleared and we reopen the picker — a
		// selection re-plans and lands back on review, ready to retry. Once a diff
		// side has published, retargeting is off the table: the remaining side would
		// land in a second half-container under a different root.
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
	cmd = tea.Batch(cmd, m.syncPickerModeWidth())
	if didSelect, fppath := m.filepicker.DidSelectFile(msg); didSelect {
		if newReq, staging, final, perr := m.planOverrideAllSides(fppath); perr == nil {
			m.req = newReq
			m.staging = staging
			m.final = final
			m.state = extractStateReview
			m.filepickerErr = ""
			// planExtractOverride refuses occupied roots, so the new paths are
			// fresh by construction; the space probe answers for the new
			// filesystem.
			m.targetBusy = false
			m.targetFree, m.targetFreeKnown = app.ExtractFreeSpace(staging)
		} else {
			m.filepickerErr = firstLine(perr.Error())
		}
	} else if didSelect, _ := m.filepicker.DidSelectDisabledFile(msg); didSelect {
		m.filepickerErr = "select a directory"
	}
	return m, cmd, false
}

// planOverrideAllSides retargets the whole operation to root: the active
// request plus every queued diff side, refusing if ANY side's staging or final
// under the new root is occupied — a partially-retargetable pair would
// otherwise surface as a mid-run refusal after the first side published. On
// success the queued requests carry the new root and the diff meta's container
// display path tracks it.
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
	case entry.Type == model.NodeTypeFile:
		// A regular file restores via restic restore --include and lands at its true
		// mirror path under the per-snapshot directory.
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

// extractRequestFromFindVersion translates a find-versions row into a file
// ExtractRequest against one of the version's occurrence snapshots. The
// synthetic file entry's regular-file attestation is backed twice over:
// openBrowseVersions admits only Type == "file" rows into the view, and
// GroupFileVersions drops any match restic reports as another type (the path
// may have changed type across history) — so every extractable occurrence was
// reported as a regular file. Routing through extractRequestFromBrowseEntry
// keeps the path and slug rules from drifting between the two entry points.
func extractRequestFromFindVersion(repo, snapID, p string) (app.ExtractRequest, error) {
	return extractRequestFromBrowseEntry(repo, snapID, model.BrowseEntry{Path: p, Type: model.NodeTypeFile})
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
		return []key.Binding{helpAs(keys.Shell, "shell here"), keys.Back}
	case extractStateCanceled, extractStateError:
		if m.stagingExists {
			return []key.Binding{keys.Keep, keys.Delete}
		}
		if isExtractRefusal(m.err) && len(m.published) == 0 {
			// Advertise the retarget affordance the hint points to (handleTerminalKey
			// honors t in this branch and, like here, withholds it once a diff side
			// has already published).
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
