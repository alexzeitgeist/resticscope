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
// call order without races with the goroutine spawned by startRun.
type extractCall struct {
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

	// shellDir records the dir passed to the most recent LocalShellSession call;
	// shellErr (when set) is what that call returns instead of a session.
	shellDir string
	shellErr error
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
	f.calls = append(f.calls, extractCall{targetRoot: req.TargetRoot, source: req.Source})
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

func (f *fakeExtractDriver) LocalShellSession(dir string) (*app.ShellSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shellDir = dir
	if f.shellErr != nil {
		return nil, f.shellErr
	}
	return &app.ShellSession{Shell: "/bin/sh", Dir: dir, Cleanup: func() error { return nil }}, nil
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
		Mode:           app.ExtractFile,
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

// Directory source happy path: review → running → success. enter commits the
// live extract directly — there is no dry-run preview step.
func TestExtractDirectoryHappyPath(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()

	drv.push(extractResp{result: app.ExtractResult{Files: 2, Dirs: 1, Bytes: 4096, FinalDir: em.final, Elapsed: 2 * time.Second}})

	em, cmd, leave := dispatchKey(em, keys, "enter")
	if leave {
		t.Fatal("enter on review must not leave the modal")
	}
	if em.state != extractStateRunning {
		t.Fatalf("after enter, state = %v, want running", em.state)
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
	if len(calls) != 1 {
		t.Fatalf("Extract called %d times, want 1", len(calls))
	}
	if calls[0].source != dirReq().Source {
		t.Errorf("Extract source = %q, want %q", calls[0].source, dirReq().Source)
	}
}

// File source happy path: review → running → success. enter commits directly.
func TestExtractFileHappyPath(t *testing.T) {
	em, drv := newExtractFixture(t, fileReq())
	keys := defaultKeys()
	drv.push(extractResp{result: app.ExtractResult{Files: 1, Bytes: 412, FinalDir: em.final, Elapsed: time.Second}})

	em, cmd, _ := dispatchKey(em, keys, "enter")
	if em.state != extractStateRunning {
		t.Fatalf("after enter on file review, state = %v, want running", em.state)
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
	if calls[0].source != fileReq().Source {
		t.Errorf("Extract source = %q, want %q", calls[0].source, fileReq().Source)
	}
}

// File layout toggle: `f` on the review screen flips Nested, switching the
// rendered Final path from the flattened default (final/<name>) to nested
// (final/etc/<name>). The `?` help overlay's Extract section advertises the same
// layout key/label, so the inline help and the overlay stay in sync.
func TestExtractFileLayoutToggle(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), fileReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateReview // file source review screen carries the layout toggle
	keys := defaultKeys()

	render := func(em extractModel) string {
		m := newTestModel(t, a)
		m.view = extractView
		m.width = 240 // wide enough that the Final path renders on one line
		m.extract = em
		return m.extractBody()
	}

	flatPath := filepath.Join(em.final, "hosts")
	nestedPath := filepath.Join(em.final, "etc", "hosts")

	// Default layout is flattened: the file lands at final/<name>.
	flatBody := render(em)
	if !strings.Contains(flatBody, "flattened") {
		t.Errorf("flattened review missing the layout label:\n%s", flatBody)
	}
	if !strings.Contains(flatBody, flatPath) {
		t.Errorf("flattened review missing the flattened final path %q:\n%s", flatPath, flatBody)
	}
	if strings.Contains(flatBody, nestedPath) {
		t.Errorf("flattened review unexpectedly showed the nested path:\n%s", flatBody)
	}

	// f flips to nested.
	em, _, leave := dispatchKey(em, keys, "f")
	if leave {
		t.Fatal("f on file review must not leave the modal")
	}
	if !em.req.Nested {
		t.Fatal("f did not flip req.Nested to true")
	}
	nestedBody := render(em)
	if !strings.Contains(nestedBody, "nested") {
		t.Errorf("nested review missing the layout label:\n%s", nestedBody)
	}
	if !strings.Contains(nestedBody, nestedPath) {
		t.Errorf("nested review missing the nested final path %q:\n%s", nestedPath, nestedBody)
	}

	// f again flips back to flattened.
	em, _, _ = dispatchKey(em, keys, "f")
	if em.req.Nested {
		t.Error("second f did not flip back to flattened")
	}

	// The ? help overlay's Extract (from browse) section carries the layout entry,
	// matching the inline helpLine hint.
	help := newTestModel(t, a).helpBody()
	if !strings.Contains(help, "file layout: flattened/nested") {
		t.Errorf("help overlay missing the layout entry:\n%s", help)
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

	// Jump straight into running with startRun.
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

	// The live run fails the fresh-target check before any staging is created, so
	// the result reports StagingCreated=false.
	cmd := em.startRun()
	em.state = extractStateRunning
	for _, msg := range runBatchLeaves(t, cmd) {
		if done, ok := msg.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	if em.state != extractStateError {
		t.Fatalf("after error msg, state = %v, want error", em.state)
	}
	if em.result.StagingCreated {
		t.Error("an extract that returned ErrExtractFinalExists must not have StagingCreated")
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
	em.result = app.ExtractResult{FinalDir: "/some/where", StagingDir: "/staging"}
	em.err = errors.New("boom")

	em.clearTransient()

	if em.req.Source != "" || em.req.SourceName != "" || em.req.SnapshotID != "" {
		t.Errorf("clearTransient left req populated: %+v", em.req)
	}
	if em.staging != "" || em.final != "" {
		t.Errorf("clearTransient left paths: staging=%q final=%q", em.staging, em.final)
	}
	if em.result.FinalDir != "" || em.result.StagingDir != "" {
		t.Errorf("clearTransient left result paths: %+v", em.result)
	}
	if em.err != nil {
		t.Errorf("clearTransient left err = %v", em.err)
	}
}

// extractRequestFromBrowseEntry: directories produce ExtractDirectoryTree
// requests, files produce ExtractFile (defaulting to the flattened layout),
// unsupported types return the sentinel.
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
	if got.Mode != app.ExtractFile || !got.WasRegularFile {
		t.Errorf("file mode/regular = %v/%v, want ExtractFile / true", got.Mode, got.WasRegularFile)
	}
	if got.Nested {
		t.Error("a fresh file request must default to flattened (Nested=false)")
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

// The success screen appends a count-only warning when the extracted tree
// carried unsafe symlinks, phrased for the active policy — and leaks no path or
// link name (only the count).
func TestExtractSuccessShowsUnsafeSymlinkWarning(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{
		Files:               2,
		FinalDir:            "/extracted/here",
		UnsafeSymlinks:      3,
		UnsafeSymlinkPolicy: "keep",
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240 // wide enough that the warning line is not clipped
	m.extract = em
	body := m.extractBody()

	if !strings.Contains(body, "3 unsafe symlinks") {
		t.Errorf("success view missing the unsafe-symlink count:\n%s", body)
	}
	if !strings.Contains(body, "left in place") {
		t.Errorf("keep-policy warning text missing:\n%s", body)
	}
	if strings.Contains(body, dirReq().Source) {
		t.Errorf("success view leaked the source path:\n%s", body)
	}
}

// The success-view `s` action opens a credential-free shell rooted at the
// extracted directory via the driver's LocalShellSession.
func TestExtractSuccessShellHere(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	em.state = extractStateSuccess
	em.result = app.ExtractResult{FinalDir: "/extracted/here"}

	next, cmd, leave := dispatchKey(em, defaultKeys(), "s")
	if leave {
		t.Error("s must not leave the modal; the success screen stays up under the shell")
	}
	if next.state != extractStateSuccess {
		t.Errorf("state = %v after s, want success", next.state)
	}
	if cmd == nil {
		t.Fatal("s produced no command")
	}
	if drv.shellDir != "/extracted/here" {
		t.Errorf("LocalShellSession dir = %q, want the final extracted dir", drv.shellDir)
	}
	// Do not run cmd: it is tea.ExecProcess wrapping a real shell exec.
}

// A LocalShellSession failure surfaces as a path-free shellExitedMsg rather than
// being swallowed, and leaves the user on the success screen.
func TestExtractSuccessShellHereError(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	em.state = extractStateSuccess
	em.result = app.ExtractResult{FinalDir: "/extracted/here"}
	drv.shellErr = errors.New("local shell: invalid working directory: dir not a directory")

	next, cmd, _ := dispatchKey(em, defaultKeys(), "s")
	if next.state != extractStateSuccess {
		t.Errorf("state = %v after failed s, want success", next.state)
	}
	if cmd == nil {
		t.Fatal("s produced no command on the error path")
	}
	msg, ok := cmd().(shellExitedMsg)
	if !ok {
		t.Fatalf("error path produced %T, want shellExitedMsg", cmd())
	}
	if msg.err == nil {
		t.Fatal("shellExitedMsg carried no error")
	}
	if strings.Contains(msg.err.Error(), "/extracted/here") {
		t.Errorf("shell error leaked the destination path: %v", msg.err)
	}
}

// End-to-end regression lock for the one behaviour step 06 exists for: pressing
// `s` on the success screen must launch the shell *in the extracted directory*.
// It drives the real *app.App through handleSuccessKey → LocalShellSession →
// shellCmdFromSession → exec.Cmd.Dir → a fake shell that records its actual cwd,
// mirroring TestDetailShellKeyRoutePassesSnapshot. A reflected exec.Cmd.Dir
// assertion would couple to bubbletea's internal exec wrapper; running the shell
// and reading its working directory proves the same contract via the public
// ExecCommand.Run seam.
func TestExtractSuccessShellHereOpensInFinalDir(t *testing.T) {
	finalDir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "pwd")
	shellPath := filepath.Join(t.TempDir(), "fakeshell")
	// Pass the output path through a preserved env var and quote it in the script,
	// so a TMPDIR containing a space or shell metacharacter can't break the
	// redirection. TEST_PWD_OUT is a generic var (not a RESTIC_/AWS_/B2_/
	// RESTICSCOPE_ family member), so it survives stripCredEnv and reaches the
	// child — which also confirms the filter doesn't over-strip ordinary env.
	t.Setenv("TEST_PWD_OUT", outPath)
	script := "#!/bin/sh\npwd -P > \"$TEST_PWD_OUT\"\nexit 0\n"
	if err := os.WriteFile(shellPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}

	a := extractApp(t)
	a.Cfg.Global.Shell = shellPath // LocalShellSession resolves this shell

	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{FinalDir: finalDir}

	_, cmd, _ := dispatchKey(em, defaultKeys(), "s")
	if cmd == nil {
		t.Fatal("s produced no command")
	}
	// extractExecCommand (routing_test.go) unwraps the tea.ExecProcess wrapper so
	// we can Run() the fake shell directly without driving a real tea.Program.
	ec := extractExecCommand(t, cmd())
	if err := ec.Run(); err != nil {
		t.Fatalf("ExecCommand.Run: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read fake shell cwd: %v", err)
	}
	// Compare against the symlink-resolved target: t.TempDir paths can sit under a
	// symlinked root on some platforms, and `pwd -P` reports the physical path.
	want, err := filepath.EvalSymlinks(finalDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if dir := strings.TrimSpace(string(got)); dir != want {
		t.Errorf("shell launched in %q, want the extracted dir %q", dir, want)
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
	cmd := em.startRun()
	em.state = extractStateRunning
	for _, msg := range runBatchLeaves(t, cmd) {
		if done, ok := msg.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
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

// esc from review leaves the modal and the root's clearTransient zeros every
// transient field. This exercises the real key-routing path rather than calling
// clearTransient directly.
func TestExtractEscFromReviewClears(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, context.Background(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em

	// esc on review leaves; run the returned back-to-browse cmd.
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
		got, err := app.SanitizeExtractSlug(c.in)
		if err != nil {
			t.Errorf("app.SanitizeExtractSlug(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("app.SanitizeExtractSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := app.SanitizeExtractSlug(""); err == nil {
		t.Error("app.SanitizeExtractSlug(\"\") should return an error")
	}

	// Genuine anti-drift cross-check: a SourceName produced by the TUI slug must
	// be accepted by app.PlanExtractPaths, which independently re-derives the
	// wanted slug from Source. If the two rules ever drift, the app layer rejects
	// the request with a source_name error and this loop catches it — a guarantee
	// the hardcoded table above cannot give on its own.
	cfg, _ := extractCfg(t)
	const snap = "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"
	for _, base := range []string{"nginx", "my dir", "weird@!name", "a.b-c_d", "café", "...trim"} {
		name, err := app.SanitizeExtractSlug(base)
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
