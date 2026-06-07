package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// extract_test.go drives the extract sub-model through every state transition
// described in step 05's test plan: directory + file happy paths, no-op keys
// that must not start work, target-root override re-plan, cancel + keep/delete,
// error path with and without staging, generation-token discards, and the
// clear-on-leave non-negotiable.

// --- fakes ---

// extractCall records one Extract call so a test can assert request shape and
// call order without races with the goroutine spawned by startRun / startDryRun.
type extractCall struct {
	dryRun     bool
	targetRoot string
	source     string
}

// fakeExtractDriver is the test double for extractDriver. result/err/onResult/
// blockUntil let a test program the next response; calls/cancels record every
// invocation for assertions. The driver lives entirely in the test goroutine —
// no extra synchronization is needed because the sub-model invokes it on the
// Cmd thread, and the tests reach a steady state by calling cmd() directly
// rather than running the Bubble Tea program loop.
type fakeExtractDriver struct {
	mu       sync.Mutex
	calls    []extractCall
	queue    []extractResp
	canceled int
}

type extractResp struct {
	result   app.ExtractResult
	err      error
	progress []app.ExtractProgress
	blockOn  <-chan struct{} // when set, Extract blocks until either this closes or ctx fires
}

func (f *fakeExtractDriver) push(r extractResp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, r)
}

func (f *fakeExtractDriver) Extract(ctx context.Context, req app.ExtractRequest, onProgress func(app.ExtractProgress)) (app.ExtractResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, extractCall{dryRun: req.DryRun, targetRoot: req.TargetRoot, source: req.Source})
	var resp extractResp
	if len(f.queue) > 0 {
		resp = f.queue[0]
		f.queue = f.queue[1:]
	}
	f.mu.Unlock()
	for _, p := range resp.progress {
		if onProgress != nil {
			onProgress(p)
		}
	}
	if resp.blockOn != nil {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.canceled++
			f.mu.Unlock()
			return resp.result, ctx.Err()
		case <-resp.blockOn:
			return resp.result, resp.err
		}
	}
	return resp.result, resp.err
}

func (f *fakeExtractDriver) callsSnapshot() []extractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]extractCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- helpers ---

// extractCfg returns an Extract config rooted at a per-test temp dir, so the
// target_root override and PlanExtractPaths checks have a real absolute path to
// work with.
func extractCfg(t *testing.T) (config.Extract, string) {
	t.Helper()
	root := t.TempDir()
	return config.Extract{
		TargetRoot:     root,
		ExtractTimeout: config.Duration(5 * time.Minute),
	}, root
}

func extractApp(t *testing.T) *app.App {
	t.Helper()
	cfg, _ := extractCfg(t)
	a := testApp(nil)
	a.Cfg.Extract = cfg
	return a
}

// dirReq / fileReq build valid ExtractRequests against the seeded repo-a.
func dirReq() app.ExtractRequest {
	return app.ExtractRequest{
		Repo:           "repo-a",
		SnapshotID:     "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011",
		SnapshotShort:  "a1b2c3d4",
		Source:         "/etc/nginx",
		SourceName:     "nginx",
		Mode:           app.ExtractDirectoryTree,
		WasRegularFile: false,
	}
}

func fileReq() app.ExtractRequest {
	return app.ExtractRequest{
		Repo:           "repo-a",
		SnapshotID:     "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011",
		SnapshotShort:  "a1b2c3d4",
		Source:         "/etc/hosts",
		SourceName:     "hosts",
		Mode:           app.ExtractFileBytes,
		WasRegularFile: true,
	}
}

// newExtractFixture installs a fake driver onto a freshly-built extractModel,
// returning the model and the driver for assertions.
func newExtractFixture(t *testing.T, req app.ExtractRequest) (extractModel, *fakeExtractDriver) {
	t.Helper()
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), req, 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	drv := &fakeExtractDriver{}
	em.drv = drv
	return em, drv
}

// runCmd invokes a Bubble Tea Cmd synchronously and returns the produced Msg.
// Returns nil for nil Cmd. Batches are flattened to their first leaf for the
// shapes we test here (worker + progress pump come back as a Batch).
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		// Drive the first leaf — that's the worker Cmd in startRun.
		for _, c := range batch {
			if m := c(); m != nil {
				return m
			}
		}
		return nil
	}
	return msg
}

// runBatchLeaves invokes every leaf in a (possibly batched) Cmd, returning the
// produced Msgs in order. Skips nils. Used by tests that need both the worker
// done msg and the first progress msg.
func runBatchLeaves(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	first := cmd()
	batch, ok := first.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{first}
	}
	var out []tea.Msg
	for _, c := range batch {
		if m := c(); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// dispatchKey passes a key through handleKey, returns the new model, the Cmd,
// and the "leave modal" flag.
func dispatchKey(em extractModel, keys keyMap, k string) (extractModel, tea.Cmd, bool) {
	return em.handleKey(keys, press(k))
}

// --- tests ---

// Directory source happy path: review → preview (dry-run) → running → success.
func TestExtractDirectoryHappyPath(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()

	dryRows := []app.ExtractPreviewItem{
		{Action: "restored", Item: "./nginx.conf", Size: 2100},
		{Action: "restored", Item: "./sites-enabled/default", Size: 1800},
	}
	drv.push(extractResp{result: app.ExtractResult{Files: 2, Dirs: 1, Bytes: 4096, DryRunPreview: dryRows}})
	drv.push(extractResp{result: app.ExtractResult{Files: 2, Dirs: 1, Bytes: 4096, FinalDir: em.final, Elapsed: 2 * time.Second}})

	em, cmd, leave := dispatchKey(em, keys, "enter")
	if leave {
		t.Fatal("enter on review must not leave the modal")
	}
	msg := runCmd(t, cmd)
	em.applyDryRunDone(msg.(extractDryRunDoneMsg))
	if em.state != extractStatePreview {
		t.Fatalf("after dry-run msg, state = %v, want preview", em.state)
	}
	if len(em.previewRows) != 2 {
		t.Fatalf("previewRows = %d, want 2", len(em.previewRows))
	}

	em, cmd, leave = dispatchKey(em, keys, "g")
	if leave {
		t.Fatal("g on preview must not leave the modal")
	}
	if em.state != extractStateRunning {
		t.Fatalf("after g, state = %v, want running", em.state)
	}
	leaves := runBatchLeaves(t, cmd)
	for _, m := range leaves {
		if done, ok := m.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	if em.state != extractStateSuccess {
		t.Fatalf("after run msg, state = %v, want success", em.state)
	}

	calls := drv.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("Extract called %d times, want 2", len(calls))
	}
	if !calls[0].dryRun || calls[1].dryRun {
		t.Errorf("call dryRun flags = [%v %v], want [true false]", calls[0].dryRun, calls[1].dryRun)
	}
}

// File source happy path: review → preview (no restic call) → running → success.
func TestExtractFileHappyPath(t *testing.T) {
	em, drv := newExtractFixture(t, fileReq())
	keys := defaultKeys()
	drv.push(extractResp{result: app.ExtractResult{Files: 1, Bytes: 412, FinalDir: em.final, Elapsed: time.Second}})

	em, cmd, _ := dispatchKey(em, keys, "enter")
	if em.state != extractStatePreview {
		t.Fatalf("after enter on file review, state = %v, want preview", em.state)
	}
	if cmd != nil {
		t.Errorf("enter on file review should not start a Cmd; got %T", cmd())
	}
	if got := len(drv.callsSnapshot()); got != 0 {
		t.Fatalf("Extract called %d times before g, want 0", got)
	}

	em, cmd, _ = dispatchKey(em, keys, "g")
	if em.state != extractStateRunning {
		t.Fatalf("after g on file preview, state = %v, want running", em.state)
	}
	for _, m := range runBatchLeaves(t, cmd) {
		if done, ok := m.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	if em.state != extractStateSuccess {
		t.Fatalf("after run msg on file, state = %v, want success", em.state)
	}
	calls := drv.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("Extract called %d times, want 1", len(calls))
	}
	if calls[0].dryRun {
		t.Error("file Extract call must not be dry-run")
	}
}

// `m` is not bound to anything in the extract sub-model — it must be a no-op
// in review state.
func TestExtractIgnoresUnknownKey(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	em2, cmd, _ := dispatchKey(em, keys, "m")
	if em2.state != extractStateReview {
		t.Errorf("after m in review, state = %v, want review unchanged", em2.state)
	}
	if cmd != nil {
		t.Errorf("after m in review, cmd = %T, want nil", cmd())
	}
	if got := len(drv.callsSnapshot()); got != 0 {
		t.Errorf("after m, Extract calls = %d, want 0", got)
	}
}

// `t` opens the filepicker overlay; a selection re-plans staging/final.
func TestExtractTargetKeyOpensFilePicker(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	em, _, leave := dispatchKey(em, keys, "t")
	if leave {
		t.Fatal("t on review must not leave the modal")
	}
	if em.state != extractStateFilePicker {
		t.Fatalf("after t, state = %v, want filepicker", em.state)
	}
}

// Regression for the async filepicker: its Init / navigation commands produce an
// unexported readDirMsg, and the root Model must forward that message to the
// picker (Update's default branch) or the directory list renders forever empty.
func TestExtractFilePickerReceivesAsyncDirMsg(t *testing.T) {
	a := extractApp(t)
	root := a.Cfg.Extract.TargetRoot
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.height = 24

	// `t` opens the picker and returns the Init command that reads the root dir.
	em, cmd, _ := dispatchKey(em, defaultKeys(), "t")
	if em.state != extractStateFilePicker {
		t.Fatalf("t did not open the filepicker; state = %v", em.state)
	}
	msg := runCmd(t, cmd)
	if msg == nil {
		t.Fatal("opening the filepicker produced no readDir command")
	}

	// Route the async dir message through the real Model.Update default branch.
	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em
	m = update(t, m, msg)

	if body := m.extractFilePickerBody(80); !strings.Contains(body, "subdir") {
		t.Fatalf("filepicker did not load directory contents after async msg; body = %q", body)
	}
}

// planExtractOverride rejects an existing target and accepts a fresh one.
func TestExtractPlanOverrideRejectsExisting(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	// Pre-create the final directory under a fresh root so the re-plan refuses.
	existing := t.TempDir()
	repoDir := filepath.Join(existing, "repo-a")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, _, _, err := planExtractOverride(em.cfg, em.req, "not-absolute"); err == nil {
		t.Error("planExtractOverride with non-absolute root should fail")
	}
	// A real fresh root: should succeed and the final must lie under it.
	newReq, staging, final, err := planExtractOverride(em.cfg, em.req, existing)
	if err != nil {
		t.Fatalf("planExtractOverride: %v", err)
	}
	if newReq.TargetRoot != existing {
		t.Errorf("newReq.TargetRoot = %q, want %q", newReq.TargetRoot, existing)
	}
	if !strings.HasPrefix(final, existing) {
		t.Errorf("final = %q, want under %q", final, existing)
	}
	if !strings.HasPrefix(staging, existing) {
		t.Errorf("staging = %q, want under %q", staging, existing)
	}

	// Pre-create the final dir and verify the override is now rejected.
	if err := os.MkdirAll(final, 0o700); err != nil {
		t.Fatalf("MkdirAll final: %v", err)
	}
	if _, _, _, err := planExtractOverride(em.cfg, em.req, existing); err == nil {
		t.Error("planExtractOverride should reject when final already exists")
	}
}

// Cancel during running: esc triggers the per-op cancel; the worker returns
// context.Canceled; the sub-model lands on canceled state with the keep/delete
// prompt only when StagingCreated=true.
func TestExtractCancelOffersKeepOrDelete(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	block := make(chan struct{})
	// Simulate the staging dir's existence so the keep/delete branch is taken.
	stagingDir := em.staging
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("MkdirAll staging: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingDir) })

	drv.push(extractResp{
		result:  app.ExtractResult{StagingDir: stagingDir, StagingCreated: true},
		err:     context.Canceled,
		blockOn: block,
	})

	// Skip the dry-run dance by jumping straight into running with startRun.
	cmd := em.startRun()
	em.state = extractStateRunning

	// Press esc → handleRunningKey calls cancel(); the worker observes ctx.Done.
	em, _, _ = em.back(keys)
	// Drain the worker leaf (it returns context.Canceled now that ctx fired).
	for _, msg := range runBatchLeaves(t, cmd) {
		if done, ok := msg.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	close(block)

	if em.state != extractStateCanceled {
		t.Fatalf("after cancel, state = %v, want canceled", em.state)
	}
	// `d` in canceled state requests deletion.
	em, cmd, _ = dispatchKey(em, keys, "d")
	if em.state != extractStateKeepDelete {
		t.Fatalf("after d, state = %v, want keep-delete", em.state)
	}
	msg, ok := cmd().(extractDeleteStagingDoneMsg)
	if !ok {
		t.Fatalf("delete cmd returned %T, want extractDeleteStagingDoneMsg", cmd())
	}
	em.applyDeleteStagingDone(msg)
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Errorf("after d, staging dir still present: stat err = %v", err)
	}
}

// `k` (keep) and esc both close the keep-or-delete prompt without removing
// staging.
func TestExtractKeepLeavesStagingOnDisk(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	staging := em.staging
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })
	em.state = extractStateCanceled
	em.result = app.ExtractResult{StagingDir: staging, StagingCreated: true}

	em2, cmd, leave := dispatchKey(em, keys, "k")
	if !leave {
		t.Error("k on canceled with staging should leave the modal")
	}
	if cmd == nil {
		t.Fatal("k should produce a back-to-browse cmd")
	}
	if _, ok := cmd().(extractBackToBrowseMsg); !ok {
		t.Errorf("k cmd returned %T, want extractBackToBrowseMsg", cmd())
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("k must not remove staging; stat err = %v", err)
	}
	_ = em2
}

// Error path with StagingCreated=false must not show the keep-or-delete prompt;
// enter/esc returns to browse.
func TestExtractErrorWithoutStagingClosesDirectly(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	drv.push(extractResp{err: app.ErrExtractFinalExists})

	// Drive straight to startDryRun → applyDryRunDone path so we land on an error
	// before any staging is created.
	cmd := em.startDryRun()
	em.applyDryRunDone(runCmd(t, cmd).(extractDryRunDoneMsg))
	if em.state != extractStateError {
		t.Fatalf("after error msg, state = %v, want error", em.state)
	}
	if em.result.StagingCreated {
		t.Error("dry-run that returned ErrExtractFinalExists must not have StagingCreated")
	}

	em, cmd, leave := dispatchKey(em, keys, "enter")
	if !leave {
		t.Error("enter on no-staging error must leave the modal")
	}
	if _, ok := cmd().(extractBackToBrowseMsg); !ok {
		t.Errorf("enter cmd returned %T, want extractBackToBrowseMsg", cmd())
	}
}

// gen check: stale extractRunDoneMsg from a previous run is discarded.
func TestExtractStaleRunDoneDropped(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	em.state = extractStateRunning
	em.gen = 5

	em.applyRunDone(extractRunDoneMsg{gen: 4, err: nil, result: app.ExtractResult{FinalDir: "/should/not/leak"}})
	if em.state != extractStateRunning {
		t.Errorf("stale msg flipped state to %v, want running", em.state)
	}
	if em.result.FinalDir != "" {
		t.Errorf("stale msg leaked FinalDir = %q", em.result.FinalDir)
	}

	em.applyRunDone(extractRunDoneMsg{gen: 5, result: app.ExtractResult{FinalDir: "/ok"}})
	if em.state != extractStateSuccess {
		t.Errorf("current-gen msg did not advance state; state = %v", em.state)
	}
}

// clearTransient zeros every path-bearing field. Built directly because Model
// owns the call site and tests can hit the helper without a full route.
func TestExtractClearTransientZerosPaths(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	em.previewRows = []app.ExtractPreviewItem{{Action: "restored", Item: "./x", Size: 1}}
	em.previewSummary = app.ExtractResult{Files: 1}
	em.result = app.ExtractResult{FinalDir: "/some/where", StagingDir: "/staging"}
	em.err = errors.New("boom")

	em.clearTransient()

	if em.req.Source != "" || em.req.SourceName != "" || em.req.SnapshotID != "" {
		t.Errorf("clearTransient left req populated: %+v", em.req)
	}
	if em.staging != "" || em.final != "" {
		t.Errorf("clearTransient left paths: staging=%q final=%q", em.staging, em.final)
	}
	if em.previewRows != nil || em.previewSummary.Files != 0 {
		t.Errorf("clearTransient left preview state")
	}
	if em.result.FinalDir != "" || em.result.StagingDir != "" {
		t.Errorf("clearTransient left result paths: %+v", em.result)
	}
	if em.err != nil {
		t.Errorf("clearTransient left err = %v", em.err)
	}
}

// extractRequestFromBrowseEntry: directories produce ExtractDirectoryTree
// requests, files produce ExtractFileBytes, unsupported types return the
// sentinel.
func TestExtractRequestFromBrowseEntry(t *testing.T) {
	snap := "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"

	dir := model.BrowseEntry{Path: "/etc/nginx", Name: "nginx", Type: "dir", IsDir: true}
	got, err := extractRequestFromBrowseEntry("repo-a", snap, dir)
	if err != nil {
		t.Fatalf("dir entry: %v", err)
	}
	if got.Mode != app.ExtractDirectoryTree {
		t.Errorf("dir mode = %v, want directory tree", got.Mode)
	}
	if got.SourceName != "nginx" {
		t.Errorf("dir SourceName = %q, want %q", got.SourceName, "nginx")
	}

	file := model.BrowseEntry{Path: "/etc/hosts", Name: "hosts", Type: "file"}
	got, err = extractRequestFromBrowseEntry("repo-a", snap, file)
	if err != nil {
		t.Fatalf("file entry: %v", err)
	}
	if got.Mode != app.ExtractFileBytes || !got.WasRegularFile {
		t.Errorf("file mode/regular = %v/%v, want file-bytes / true", got.Mode, got.WasRegularFile)
	}

	weird := model.BrowseEntry{Path: "/dev/null", Name: "null", Type: "char"}
	if _, err := extractRequestFromBrowseEntry("repo-a", snap, weird); !errors.Is(err, ErrExtractUnsupportedType) {
		t.Errorf("weird type err = %v, want ErrExtractUnsupportedType", err)
	}
}

// Opening help over a live extract must not drop its completion: help is an
// overlay that returns to extractView via prevView (not a real exit), so an
// extractRunDoneMsg landing while help is open has to be honored, otherwise the
// modal is stranded in extractStateRunning after a finished extract.
func TestExtractHelpOverlayKeepsRunDone(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateRunning
	em.gen = 1

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em

	// Open help over the running extract.
	m = update(t, m, press("?"))
	if m.view != helpView || m.prevView != extractView {
		t.Fatalf("? did not open help over extract: view=%v prevView=%v", m.view, m.prevView)
	}

	// The worker completes while help is up.
	m = update(t, m, extractRunDoneMsg{gen: 1, result: app.ExtractResult{Files: 2, FinalDir: "/ok"}})

	// Close help; we must land back on extract in the success state, not stranded
	// in running.
	m = update(t, m, press("?"))
	if m.view != extractView {
		t.Fatalf("closing help did not return to extract: view=%v", m.view)
	}
	if m.extract.state != extractStateSuccess {
		t.Fatalf("extract stranded in %v after run completed under help; want success", m.extract.state)
	}
}

// A long dry-run preview item path must wrap onto continuation lines, never be
// truncated — this is the screen where the user inspects what will be extracted.
func TestExtractPreviewWrapsLongItemPath(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStatePreview
	long := "./etc/nginx/sites-available/deeply/nested/path/to/a/config-file-with-a-very-long-name.conf"
	em.previewRows = []app.ExtractPreviewItem{{Action: "restored", Item: long}}
	em.previewSummary = app.ExtractResult{Files: 1}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 60 // narrow enough that the path must wrap
	m.height = 24
	m.extract = em

	body := m.extractBody()

	// No single line carries the whole path (proves it isn't on one line)...
	for _, ln := range strings.Split(body, "\n") {
		if strings.Contains(ln, long) {
			t.Fatalf("long item path rendered on a single line (not wrapped):\n%q", ln)
		}
	}
	// ...yet every character of the path is present once whitespace/newlines are
	// stripped (proves it was wrapped, not elided).
	stripWS := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\t' {
				return -1
			}
			return r
		}, s)
	}
	if !strings.Contains(stripWS(body), long) {
		t.Fatalf("long item path was elided rather than wrapped:\n%s", body)
	}
}

// Privacy on render: the cancel/error screens must never echo the source path,
// and the staging path appears only alongside the keep/delete prompt.
func TestExtractTerminalViewHidesSourcePath(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	staging := em.staging
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("MkdirAll staging: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })

	// Canceled with a staging dir this run created → prompt + staging path shown,
	// source path never shown.
	em.state = extractStateCanceled
	em.err = context.Canceled
	em.result = app.ExtractResult{StagingDir: staging, StagingCreated: true}
	em.progress = app.ExtractProgress{FilesDone: 3, FilesTotal: 10, BytesDone: 1024}

	m := newTestModel(t, a)
	m.view = extractView
	// Wide enough that the staging path renders on one line (it wraps on narrow
	// terminals per §14), so the assertion can match the unbroken path.
	m.width = 240
	m.extract = em
	body := m.extractBody()
	if strings.Contains(body, dirReq().Source) {
		t.Errorf("cancel view leaked source path %q:\n%s", dirReq().Source, body)
	}
	if !strings.Contains(body, staging) {
		t.Errorf("cancel view with a created staging dir should show the staging path:\n%s", body)
	}

	// Error without staging → no keep/delete prompt, no staging path, no source.
	em.state = extractStateError
	em.err = app.ErrExtractFinalExists
	em.result = app.ExtractResult{}
	m.extract = em
	body = m.extractBody()
	if strings.Contains(body, dirReq().Source) {
		t.Errorf("error view leaked source path:\n%s", body)
	}
	if strings.Contains(body, "Staging output") {
		t.Errorf("error without created staging must not show the keep/delete prompt:\n%s", body)
	}
}

// A pre-existing staging dir (StagingCreated=false) must not be offered for
// deletion even though a matching path is on disk: this run does not own it.
func TestExtractPreExistingStagingNotDeletable(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	if err := os.MkdirAll(em.staging, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(em.staging) })

	// App.Extract refuses with ErrExtractStagingExists before any mkdir, so the
	// result reports StagingCreated=false and an empty StagingDir.
	drv.push(extractResp{err: app.ErrExtractStagingExists})
	cmd := em.startDryRun()
	em.applyDryRunDone(runCmd(t, cmd).(extractDryRunDoneMsg))
	if em.state != extractStateError {
		t.Fatalf("state = %v, want error", em.state)
	}
	if strings.Contains(em.helpLine(keys), "delete") {
		t.Errorf("help line offered delete for a non-owned staging dir: %q", em.helpLine(keys))
	}
	em2, _, _ := dispatchKey(em, keys, "d")
	if em2.state == extractStateKeepDelete {
		t.Error("d must not start a delete when staging is not owned by this run")
	}
	if _, err := os.Stat(em.staging); err != nil {
		t.Errorf("pre-existing staging must remain on disk: %v", err)
	}
}

// gen check on dry-run: a stale extractDryRunDoneMsg from a superseded run is
// discarded; the current-gen message advances to preview.
func TestExtractStaleDryRunDropped(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	em.state = extractStateReview
	em.dryRunning = true
	em.gen = 7

	em.applyDryRunDone(extractDryRunDoneMsg{
		gen:    6,
		result: app.ExtractResult{DryRunPreview: []app.ExtractPreviewItem{{Item: "./leak"}}},
	})
	if em.state != extractStateReview {
		t.Errorf("stale dry-run changed state to %v", em.state)
	}
	if len(em.previewRows) != 0 {
		t.Errorf("stale dry-run leaked %d preview rows", len(em.previewRows))
	}

	em.applyDryRunDone(extractDryRunDoneMsg{
		gen:    7,
		result: app.ExtractResult{DryRunPreview: []app.ExtractPreviewItem{{Item: "./ok"}}},
	})
	if em.state != extractStatePreview || len(em.previewRows) != 1 {
		t.Errorf("current-gen dry-run did not advance: state=%v rows=%d", em.state, len(em.previewRows))
	}
}

// esc from the dry-run preview returns to review (dropping rows); a second esc
// leaves the modal and the root's clearTransient zeros every transient field.
// This exercises the real key-routing path rather than calling clearTransient
// directly.
func TestExtractEscFromPreviewReturnsThenClears(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStatePreview
	em.previewRows = []app.ExtractPreviewItem{{Action: "restored", Item: "./a"}, {Action: "restored", Item: "./b"}}
	em.previewSummary = app.ExtractResult{Files: 2}
	em.previewScrollOffset = 1

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em

	// First esc: preview → review, rows dropped.
	next, _ := m.Update(press("esc"))
	m = next.(Model)
	if m.extract.state != extractStateReview {
		t.Fatalf("after esc, state = %v, want review", m.extract.state)
	}
	if m.extract.previewRows != nil {
		t.Error("esc from preview did not drop preview rows")
	}

	// Second esc: review → leave; run the returned back-to-browse cmd.
	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on review should return a back-to-browse cmd")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.view != browseView {
		t.Errorf("after leaving extract, view = %v, want browse", m.view)
	}
	if m.extract.req.Source != "" || m.extract.staging != "" || m.extract.final != "" {
		t.Errorf("clearTransient left paths populated: %+v", m.extract)
	}
}

// sanitizeSlug enforces the same rule as the app layer's sanitizeExtractSlug
// (verified by routing through PlanExtractPaths under the cover, which would
// otherwise reject SourceName).
func TestExtractSanitizeSlugMatchesAppLayer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nginx", "nginx"},
		{"my dir", "my-dir"},
		{".hidden", "hidden"},
		{"weird@!name", "weird-name"},
	}
	for _, c := range cases {
		got, err := sanitizeSlug(c.in)
		if err != nil {
			t.Errorf("sanitizeSlug(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("sanitizeSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := sanitizeSlug(""); err == nil {
		t.Error("sanitizeSlug(\"\") should return an error")
	}

	// Genuine anti-drift cross-check: a SourceName produced by the TUI slug must
	// be accepted by app.PlanExtractPaths, which independently re-derives the
	// wanted slug from Source. If the two rules ever drift, the app layer rejects
	// the request with a source_name error and this loop catches it — a guarantee
	// the hardcoded table above cannot give on its own.
	cfg, _ := extractCfg(t)
	const snap = "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"
	for _, base := range []string{"nginx", "my dir", "weird@!name", "a.b-c_d", "café", "...trim"} {
		name, err := sanitizeSlug(base)
		if err != nil {
			continue // unusable basenames are surfaced to the user, never extracted
		}
		req := app.ExtractRequest{
			Repo:          "repo-a",
			SnapshotID:    snap,
			SnapshotShort: snap[:8],
			Source:        "/parent/" + base,
			SourceName:    name,
			Mode:          app.ExtractDirectoryTree,
		}
		if _, _, err := app.PlanExtractPaths(cfg, req); err != nil {
			t.Errorf("app layer rejected TUI slug for base %q (slug rules drifted?): %v", base, err)
		}
	}
}
