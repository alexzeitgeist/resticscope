package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/theme"

	tea "charm.land/bubbletea/v2"
)

// These tests cover extraction state transitions, target safety, cancellation,
// privilege flow, and path-clearing contracts.

// extractCall records one extraction invocation.
type extractCall struct {
	targetRoot string
	source     string
	privileged bool
}

// fakeExtractDriver queues responses and records calls from synchronously driven commands.
type fakeExtractDriver struct {
	mu       sync.Mutex
	calls    []extractCall
	queue    []extractResp
	canceled int

	shellDir string
	shellErr error

	probeErr   error
	probeCalls int

	subFiles, subDirs int
	subKnown          bool
	subErr            error
}

type extractResp struct {
	result   app.ExtractResult
	err      error
	progress []app.ExtractProgress
	blockOn  <-chan struct{}
}

func (f *fakeExtractDriver) push(r extractResp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, r)
}

func (f *fakeExtractDriver) Extract(ctx context.Context, req app.ExtractRequest, onProgress func(app.ExtractProgress)) (app.ExtractResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, extractCall{targetRoot: req.TargetRoot, source: req.Source, privileged: req.Privileged})
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

func (f *fakeExtractDriver) PrivilegedExtractProbe(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	return f.probeErr
}

func (f *fakeExtractDriver) PrivilegedAuthCommand() *exec.Cmd {
	// Non-nil selects the interactive branch; tests never execute it.
	return exec.Command("true")
}

func (f *fakeExtractDriver) SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return 0, 0, false, f.subErr
	}
	return f.subFiles, f.subDirs, f.subKnown, nil
}

func (f *fakeExtractDriver) callsSnapshot() []extractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]extractCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// extractCfg uses a real per-test absolute target root.
func extractCfg(t *testing.T) (config.Extract, string) {
	t.Helper()
	root := t.TempDir()
	return config.Extract{
		TargetRoot:     root,
		ExtractTimeout: config.Duration(5 * time.Minute),
		RememberTarget: true,
	}, root
}

func extractApp(t *testing.T) *app.App {
	t.Helper()
	cfg, _ := extractCfg(t)
	a := testApp(nil)
	a.Cfg.Extract = cfg
	return a
}

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

// newExtractFixture builds a valid modal with its fake driver.
func newExtractFixture(t *testing.T, req app.ExtractRequest) (extractModel, *fakeExtractDriver) {
	t.Helper()
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), req, 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	drv := &fakeExtractDriver{}
	em.drv = drv
	return em, drv
}

// runCmd synchronously returns the first non-nil command result.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if m := c(); m != nil {
				return m
			}
		}
		return nil
	}
	return msg
}

// runBatchLeaves returns every non-nil result from a possibly batched command.
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

func dispatchKey(em extractModel, keys keyMap, k string) (extractModel, tea.Cmd, bool) {
	return em.handleKey(keys, press(k))
}

// footHelp flattens footer bindings for plain-text assertions.
func footHelp(em extractModel, keys keyMap) string {
	var parts []string
	for _, b := range em.shortHelp(keys) {
		parts = append(parts, b.Help().Key+" "+b.Help().Desc)
	}
	return strings.Join(parts, " · ")
}

// Directory extraction commits directly from review to a live run.
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

// Root Update must forward the file picker's asynchronous directory message.
func TestExtractFilePickerReceivesAsyncDirMsg(t *testing.T) {
	a := extractApp(t)
	root := a.Cfg.Extract.TargetRoot
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.height = 24

	em, cmd, _ := dispatchKey(em, defaultKeys(), "t")
	if em.state != extractStateFilePicker {
		t.Fatalf("t did not open the filepicker; state = %v", em.state)
	}
	msg := runCmd(t, cmd)
	if msg == nil {
		t.Fatal("opening the filepicker produced no readDir command")
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em
	m = update(t, m, msg)

	if body := m.extractFilePickerBody(80); !strings.Contains(body, "subdir") {
		t.Fatalf("filepicker did not load directory contents after async msg; body = %q", body)
	}
}

// Mode-width scans run asynchronously and reject results from directories already left.
func TestExtractPickerModeWidthAsync(t *testing.T) {
	a := extractApp(t)
	if err := os.Mkdir(filepath.Join(a.Cfg.Extract.TargetRoot, "subdir"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.height = 24

	em, cmd, _ := dispatchKey(em, defaultKeys(), "t")
	var wm extractPickerModeWidthMsg
	found := false
	for _, msg := range runBatchLeaves(t, cmd) {
		if w, ok := msg.(extractPickerModeWidthMsg); ok {
			wm, found = w, true
		}
	}
	if !found {
		t.Fatal("opening the filepicker produced no mode-width scan command")
	}
	if wm.dir != em.filepicker.CurrentDirectory {
		t.Fatalf("scan dir = %q, want %q", wm.dir, em.filepicker.CurrentDirectory)
	}
	if wm.w < 10 {
		t.Fatalf("scanned width = %d, want >= 10", wm.w)
	}
	em, _ = em.updateFilePicker(wm)
	if em.pickerModeW != wm.w {
		t.Errorf("pickerModeW = %d after width msg, want %d", em.pickerModeW, wm.w)
	}
	em, _ = em.updateFilePicker(extractPickerModeWidthMsg{dir: "/somewhere/else", w: 99})
	if em.pickerModeW == 99 {
		t.Error("width msg for a directory the picker is not in must be dropped")
	}
}

// alignFilePickerModes aligns variable-width modes on plain and selected rows.
func TestAlignFilePickerModes(t *testing.T) {
	const (
		accent = "\x1b[38;2;254;128;25m"
		reset  = "\x1b[0m"
	)
	lines := []string{
		accent + "▎" + reset + accent + " dtrwxrwxrwx     60B .ICE-unix" + reset,
		"  " + accent + "drwx------" + reset + "     60B claude-1000",
		"  -rw-r--r--      0B config-err",
		"",
		"  empty directory",
	}
	want := []string{
		lines[0],
		"  " + accent + "drwx------" + reset + "      60B claude-1000",
		"  -rw-r--r--       0B config-err",
		"",
		"  empty directory",
	}
	got := alignFilePickerModes(lines, 0)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// A directory-wide floor prevents column shifts when the widest row scrolls away.
func TestAlignFilePickerModesFloor(t *testing.T) {
	lines := []string{
		"  drwx------     60B claude-1000",
		"  -rw-r--r--      0B config-err",
	}
	want := []string{
		"  drwx------      60B claude-1000",
		"  -rw-r--r--       0B config-err",
	}
	got := alignFilePickerModes(lines, 11)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestExtractPlanOverrideRejectsExisting(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	existing := t.TempDir()
	repoDir := filepath.Join(existing, "repo-a")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, _, _, err := planExtractOverride(em.cfg, em.req, "not-absolute"); err == nil {
		t.Error("planExtractOverride with non-absolute root should fail")
	}
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

	if err := os.MkdirAll(final, 0o700); err != nil {
		t.Fatalf("MkdirAll final: %v", err)
	}
	if _, _, _, err := planExtractOverride(em.cfg, em.req, existing); err == nil {
		t.Error("planExtractOverride should reject when final already exists")
	}
}

// Retargeting reuses ancestors but refuses an occupied output leaf.
func TestExtractPlanOverrideAcceptsAncestorRejectsLeaf(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	root := t.TempDir()

	_, _, final, err := planExtractOverride(em.cfg, em.req, root)
	if err != nil {
		t.Fatalf("planExtractOverride (clean root): %v", err)
	}

	// Shared ancestors are reusable while the output leaf remains free.
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatalf("MkdirAll ancestor: %v", err)
	}
	if _, _, _, err := planExtractOverride(em.cfg, em.req, root); err != nil {
		t.Errorf("override should accept when only the ancestor exists: %v", err)
	}

	if err := os.MkdirAll(final, 0o700); err != nil {
		t.Fatalf("MkdirAll leaf: %v", err)
	}
	_, _, _, err = planExtractOverride(em.cfg, em.req, root)
	if err == nil {
		t.Fatal("override should reject when the exact leaf exists")
	}
	if !strings.Contains(err.Error(), "target already exists") {
		t.Errorf("reject error = %q, want it to contain \"target already exists\"", err.Error())
	}
}

// Cancellation offers cleanup only for staging created by the current run.
func TestExtractCancelOffersKeepOrDelete(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	block := make(chan struct{})
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

	cmd := em.startRun()
	em.state = extractStateRunning

	em, _, _ = em.back()
	for _, msg := range runBatchLeaves(t, cmd) {
		if done, ok := msg.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	close(block)

	if em.state != extractStateCanceled {
		t.Fatalf("after cancel, state = %v, want canceled", em.state)
	}
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

// Keeping staging closes the modal without deleting it.
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

// Errors without owned staging close directly.
func TestExtractErrorWithoutStagingClosesDirectly(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	drv.push(extractResp{err: app.ErrExtractFinalExists})

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

	em, cmd, leave := dispatchKey(em, keys, "esc")
	if !leave {
		t.Error("esc on no-staging error must leave the modal")
	}
	if _, ok := cmd().(extractBackToBrowseMsg); !ok {
		t.Errorf("esc cmd returned %T, want extractBackToBrowseMsg", cmd())
	}
}

// Enter cannot accidentally dismiss terminal screens.
func TestExtractTerminalScreensIgnoreEnter(t *testing.T) {
	keys := defaultKeys()

	em, _ := newExtractFixture(t, dirReq())
	em.state = extractStateSuccess
	em.result = app.ExtractResult{Files: 2, Dirs: 1, Bytes: 4096, FinalDir: em.final}
	em, cmd, leave := dispatchKey(em, keys, "enter")
	if leave || cmd != nil {
		t.Errorf("enter on success: leave=%v cmd=%v, want a no-op", leave, cmd != nil)
	}
	if em.state != extractStateSuccess {
		t.Errorf("enter on success: state = %v, want unchanged success", em.state)
	}
	if foot := footHelp(em, keys); !strings.Contains(foot, "q back") || strings.Contains(foot, "enter") {
		t.Errorf("success footer = %q, want 'q back' and no enter chip", foot)
	}
	em, cmd, leave = dispatchKey(em, keys, "esc")
	if !leave || cmd == nil {
		t.Fatalf("esc on success: leave=%v cmd-nil=%v, want to leave the modal", leave, cmd == nil)
	}
	if _, ok := cmd().(extractBackToBrowseMsg); !ok {
		t.Errorf("esc cmd returned %T, want extractBackToBrowseMsg", cmd())
	}

	em, _ = newExtractFixture(t, dirReq())
	em.state = extractStateError
	em.err = errors.New("boom")
	em, cmd, leave = dispatchKey(em, keys, "enter")
	if leave || cmd != nil {
		t.Errorf("enter on no-staging error: leave=%v cmd=%v, want a no-op", leave, cmd != nil)
	}
	if em.state != extractStateError {
		t.Errorf("enter on error: state = %v, want unchanged error", em.state)
	}
	if foot := footHelp(em, keys); !strings.Contains(foot, "q back") || strings.Contains(foot, "enter") {
		t.Errorf("error footer = %q, want 'q back' and no enter chip", foot)
	}
	em, cmd, leave = dispatchKey(em, keys, "esc")
	if !leave || cmd == nil {
		t.Fatalf("esc on error: leave=%v cmd-nil=%v, want to leave the modal", leave, cmd == nil)
	}
	if _, ok := cmd().(extractBackToBrowseMsg); !ok {
		t.Errorf("esc cmd returned %T, want extractBackToBrowseMsg", cmd())
	}
}

// Occupied-target errors offer retargeting; unrelated errors do not.
func TestExtractTerminalRefusalTargetReopensPicker(t *testing.T) {
	keys := defaultKeys()

	em, _ := newExtractFixture(t, dirReq())
	em.state = extractStateError
	em.err = app.ErrExtractFinalExists
	em.result = app.ExtractResult{FinalPath: em.final}

	if foot := footHelp(em, keys); !strings.Contains(foot, "target") {
		t.Errorf("terminal refusal footer missing the target affordance: %q", foot)
	}
	next, _, leave := dispatchKey(em, keys, "t")
	if leave {
		t.Fatal("t on a refusal error must not leave the modal")
	}
	if next.state != extractStateFilePicker {
		t.Fatalf("t did not reopen the filepicker; state = %v", next.state)
	}
	if next.err != nil || next.result.FinalPath != "" {
		t.Errorf("t did not clear the failed run's transient state: err=%v result=%+v", next.err, next.result)
	}

	em2, _ := newExtractFixture(t, dirReq())
	em2.state = extractStateError
	em2.err = errors.New("extract: staging metadata normalization failed")
	if foot := footHelp(em2, keys); strings.Contains(foot, "target") {
		t.Errorf("non-refusal footer should not advertise target: %q", foot)
	}
	next2, _, leave2 := dispatchKey(em2, keys, "t")
	if leave2 || next2.state != extractStateError {
		t.Errorf("t on a non-refusal error should be a no-op; state=%v leave=%v", next2.state, leave2)
	}
}

func TestExtractStaleRunDoneDropped(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	em.state = extractStateRunning
	em.gen = 5

	em.applyRunDone(extractRunDoneMsg{gen: 4, err: nil, result: app.ExtractResult{FinalDir: "/should/not/leak", FinalPath: "/should/not/leak"}})
	if em.state != extractStateRunning {
		t.Errorf("stale msg flipped state to %v, want running", em.state)
	}
	if em.result.FinalDir != "" {
		t.Errorf("stale msg leaked FinalDir = %q", em.result.FinalDir)
	}
	if em.result.FinalPath != "" {
		t.Errorf("stale msg leaked FinalPath = %q", em.result.FinalPath)
	}

	em.applyRunDone(extractRunDoneMsg{gen: 5, result: app.ExtractResult{FinalDir: "/ok", FinalPath: "/ok/file"}})
	if em.state != extractStateSuccess {
		t.Errorf("current-gen msg did not advance state; state = %v", em.state)
	}
}

// Browse entries map to directory/file modes and reject unsupported types.
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

	weird := model.BrowseEntry{Path: "/dev/null", Name: "null", Type: "char"}
	if _, err := extractRequestFromBrowseEntry("repo-a", snap, weird); !errors.Is(err, ErrExtractUnsupportedType) {
		t.Errorf("weird type err = %v, want ErrExtractUnsupportedType", err)
	}
}

// Help overlays continue accepting extraction completion messages.
func TestExtractHelpOverlayKeepsRunDone(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateRunning
	em.gen = 1

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em

	m = update(t, m, press("?"))
	if m.view != helpView || m.prevView != extractView {
		t.Fatalf("? did not open help over extract: view=%v prevView=%v", m.view, m.prevView)
	}

	m = update(t, m, extractRunDoneMsg{gen: 1, result: app.ExtractResult{Files: 2, FinalDir: "/ok"}})

	m = update(t, m, press("?"))
	if m.view != extractView {
		t.Fatalf("closing help did not return to extract: view=%v", m.view)
	}
	if m.extract.state != extractStateSuccess {
		t.Fatalf("extract stranded in %v after run completed under help; want success", m.extract.state)
	}
}

// Unsafe-symlink warnings reveal counts and policy, never paths or names.
func TestExtractSuccessShowsUnsafeSymlinkWarning(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{
		Files:          2,
		FinalDir:       "/extracted/here",
		FinalPath:      "/extracted/here",
		UnsafeSymlinks: 3,
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
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

// Narrow panes wrap unsafe-symlink warnings without losing their safety tail.
func TestExtractSuccessUnsafeSymlinkWarningReflowsWhenNarrow(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{
		Files:          2,
		FinalDir:       "/extracted/here",
		FinalPath:      "/extracted/here",
		UnsafeSymlinks: 359,
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 60
	m.extract = em
	body := m.extractBody()

	joined := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(joined, "inspect before use.") {
		t.Errorf("narrow success view clipped the warning instead of wrapping it:\n%s", body)
	}
}

// wrapWords preserves fitting text and hard-splits words that exceed the budget.
func TestWrapWords(t *testing.T) {
	for name, tc := range map[string]struct {
		s     string
		avail int
		want  string
	}{
		"fits":          {"left in place", 20, "left in place"},
		"no budget":     {"left in place", 0, "left in place"},
		"breaks":        {"targets are absolute or outside", 12, "targets are\nabsolute or\noutside"},
		"long word":     {"see /a/very/long/path now", 10, "see\n/a/very/lo\nng/path\nnow"},
		"exact fit":     {"ab cd", 5, "ab cd"},
		"boundary word": {"abcde fghij", 5, "abcde\nfghij"},
	} {
		if got := wrapWords(tc.s, tc.avail); got != tc.want {
			t.Errorf("%s: wrapWords(%q, %d) = %q, want %q", name, tc.s, tc.avail, got, tc.want)
		}
	}
}

// Singular extraction counts remain grammatical.
func TestExtractSuccessSummarySingularCounts(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{
		Files:     1,
		Dirs:      1,
		FinalDir:  "/extracted/here",
		FinalPath: "/extracted/here",
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
	m.extract = em
	if body := m.extractBody(); !strings.Contains(body, "extracted 1 file · 1 dir ·") {
		t.Errorf("success summary should use singular counts:\n%s", body)
	}
}

// Unsafe-symlink policy messages agree with singular counts.
func TestExtractUnsafeSymlinkWarningNumberAgreement(t *testing.T) {
	for policy, want := range map[string]string{
		config.UnsafeSymlinksSkip:        "1 unsafe symlink removed from the output.",
		config.UnsafeSymlinksPlaceholder: "an inert text file recording its target",
		config.UnsafeSymlinksKeep:        "its target is absolute",
	} {
		got := extractUnsafeSymlinkWarning(1, policy)
		if !strings.Contains(got, want) {
			t.Errorf("warning(1, %s) = %q, want it to contain %q", policy, got, want)
		}
		if strings.Contains(got, "symlinks") {
			t.Errorf("warning(1, %s) still uses the plural noun: %q", policy, got)
		}
	}
}

// Done-screen affordances live only in the footer.
func TestExtractDoneBodiesCarryNoKeyHints(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{Files: 2, Dirs: 1, FinalDir: "/extracted/here", FinalPath: "/extracted/here"}

	m := newTestModel(t, a)
	m.view = extractView
	m.width, m.height = 240, 50
	m.extract = em

	body := stripANSI(m.extractBody())
	if strings.Contains(body, "open a shell") || strings.Contains(body, "q back") {
		t.Errorf("success body must not embed key hints:\n%s", body)
	}
	footer := stripANSI(m.footerView())
	for _, want := range []string{"s shell here", "q back"} {
		if !strings.Contains(footer, want) {
			t.Errorf("success footer missing %q\n---\n%s", want, footer)
		}
	}

	em.state = extractStateError
	em.err = errors.New("boom")
	em.result = app.ExtractResult{}
	m.extract = em
	body = stripANSI(m.extractBody())
	if strings.Contains(body, "q back") {
		t.Errorf("error body must not embed key hints:\n%s", body)
	}
	if foot := stripANSI(m.footerView()); !strings.Contains(foot, "q back") {
		t.Errorf("error footer missing 'q back'\n---\n%s", foot)
	}
}

// Success opens a credential-free local shell at the extracted directory.
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
}

// Local-shell setup errors remain path-free and leave success visible.
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

// The real shell path must launch in the extracted directory.
func TestExtractSuccessShellHereOpensInFinalDir(t *testing.T) {
	finalDir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "pwd")
	shellPath := filepath.Join(t.TempDir(), "fakeshell")
	// A generic quoted environment variable survives credential filtering and
	// keeps metacharacters in temporary paths from breaking redirection.
	t.Setenv("TEST_PWD_OUT", outPath)
	script := "#!/bin/sh\npwd -P > \"$TEST_PWD_OUT\"\nexit 0\n"
	if err := os.WriteFile(shellPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}

	a := extractApp(t)
	a.Cfg.Global.Shell = shellPath

	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.state = extractStateSuccess
	em.result = app.ExtractResult{FinalDir: finalDir}

	_, cmd, _ := dispatchKey(em, defaultKeys(), "s")
	if cmd == nil {
		t.Fatal("s produced no command")
	}
	ec := extractExecCommand(t, cmd())
	if err := ec.Run(); err != nil {
		t.Fatalf("ExecCommand.Run: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read fake shell cwd: %v", err)
	}
	// Compare physical paths because temporary roots may contain symlinks.
	want, err := filepath.EvalSymlinks(finalDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if dir := strings.TrimSpace(string(got)); dir != want {
		t.Errorf("shell launched in %q, want the extracted dir %q", dir, want)
	}
}

// Terminal screens hide source/final paths and show staging only for cleanup.
func TestExtractTerminalViewHidesSourcePath(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	staging := em.staging
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("MkdirAll staging: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })

	// The mirrored final embeds the source, making either leak detectable.
	mirror := em.final
	if !strings.Contains(mirror, dirReq().Source) {
		t.Fatalf("test premise broken: mirror final %q does not embed the source path", mirror)
	}

	// Use the real transition to populate the cached staging probe.
	em.applyRunDone(extractRunDoneMsg{
		gen:    em.gen,
		result: app.ExtractResult{StagingDir: staging, StagingCreated: true, FinalPath: mirror},
		err:    context.Canceled,
	})
	em.progress = app.ExtractProgress{FilesDone: 3, FilesTotal: 10, BytesDone: 1024}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
	m.extract = em
	body := m.extractBody()
	if strings.Contains(body, dirReq().Source) {
		t.Errorf("cancel view leaked source path %q:\n%s", dirReq().Source, body)
	}
	if strings.Contains(body, mirror) {
		t.Errorf("cancel view leaked the source-embedding mirror final %q:\n%s", mirror, body)
	}
	if !strings.Contains(body, staging) {
		t.Errorf("cancel view with a created staging dir should show the staging path:\n%s", body)
	}

	em.applyRunDone(extractRunDoneMsg{
		gen:    em.gen,
		result: app.ExtractResult{FinalPath: mirror},
		err:    app.ErrExtractFinalExists,
	})
	m.extract = em
	body = m.extractBody()
	if strings.Contains(body, dirReq().Source) {
		t.Errorf("error view leaked source path:\n%s", body)
	}
	if strings.Contains(body, mirror) {
		t.Errorf("error view leaked the source-embedding mirror final %q:\n%s", mirror, body)
	}
	if strings.Contains(body, "Staging output") {
		t.Errorf("error without created staging must not show the keep/delete prompt:\n%s", body)
	}
	if !strings.Contains(body, "choose another target") {
		t.Errorf("error view missing the actionable refusal hint:\n%s", body)
	}
}

// Pre-existing staging is never offered for deletion because this run does not own it.
func TestExtractPreExistingStagingNotDeletable(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	if err := os.MkdirAll(em.staging, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(em.staging) })

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
	if strings.Contains(footHelp(em, keys), "delete") {
		t.Errorf("help line offered delete for a non-owned staging dir: %q", footHelp(em, keys))
	}
	em2, _, _ := dispatchKey(em, keys, "d")
	if em2.state == extractStateKeepDelete {
		t.Error("d must not start a delete when staging is not owned by this run")
	}
	if _, err := os.Stat(em.staging); err != nil {
		t.Errorf("pre-existing staging must remain on disk: %v", err)
	}
}

// The real review-exit route zeros every transient modal field.
func TestExtractEscFromReviewClears(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{}

	m := newTestModel(t, a)
	m.view = extractView
	m.extract = em

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

// TUI-produced slugs must satisfy the app layer's independent path planner.
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

	// Cross-check planner acceptance to detect slug-rule drift beyond the table.
	cfg, _ := extractCfg(t)
	const snap = "a1b2c3d4e5f67890aabbccddeeff00112233445566778899aabbccddeeff0011"
	for _, base := range []string{"nginx", "my dir", "weird@!name", "a.b-c_d", "café", "...trim"} {
		name, err := app.SanitizeExtractSlug(base)
		if err != nil {
			continue
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

// Privileged commits probe readiness before dispatching extraction.
func TestPrivilegedCommitProbeOKStartsRun(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()

	em, _, _ = dispatchKey(em, keys, "p")
	if !em.req.Privileged {
		t.Fatal("p did not toggle Privileged on")
	}

	em, cmd, _ := dispatchKey(em, keys, "enter")
	if em.state != extractStateReview || !em.isSudoBusy {
		t.Fatalf("state=%v isSudoBusy=%v, want review+busy while probing", em.state, em.isSudoBusy)
	}
	if got := len(drv.callsSnapshot()); got != 0 {
		t.Fatalf("Extract dispatched before the probe resolved: %d calls", got)
	}
	probe, ok := runCmd(t, cmd).(extractSudoProbeMsg)
	if !ok {
		t.Fatalf("commit cmd produced %T, want extractSudoProbeMsg", probe)
	}
	if drv.probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1", drv.probeCalls)
	}

	drv.push(extractResp{result: app.ExtractResult{Files: 1}})
	runCmds := em.applySudoProbe(probe)
	if em.state != extractStateRunning || em.isSudoBusy {
		t.Fatalf("state=%v isSudoBusy=%v, want running after clean probe", em.state, em.isSudoBusy)
	}
	for _, msg := range runBatchLeaves(t, runCmds) {
		if done, ok := msg.(extractRunDoneMsg); ok {
			em.applyRunDone(done)
		}
	}
	calls := drv.callsSnapshot()
	if len(calls) != 1 || !calls[0].privileged {
		t.Fatalf("calls = %+v, want one privileged Extract", calls)
	}
	if em.state != extractStateSuccess {
		t.Fatalf("state = %v, want success", em.state)
	}
}

// Failed probes route through interactive authentication before success or review.
func TestPrivilegedCommitProbeFailSudoAuthPaths(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	drv.probeErr = errors.New("sudo: a password is required")
	keys := defaultKeys()

	em, _, _ = dispatchKey(em, keys, "p")
	em, cmd, _ := dispatchKey(em, keys, "enter")
	probe := runCmd(t, cmd).(extractSudoProbeMsg)
	if authCmd := em.applySudoProbe(probe); authCmd == nil {
		t.Fatal("failed probe must return the sudo -v ExecProcess Cmd")
	}
	if em.state != extractStateReview || !em.isSudoBusy {
		t.Fatalf("state=%v isSudoBusy=%v, want review+busy during auth", em.state, em.isSudoBusy)
	}

	if c := em.applySudoAuth(extractSudoAuthMsg{gen: em.gen, err: errors.New("exit 1")}); c != nil {
		t.Fatal("auth failure must not start the run")
	}
	if em.isSudoBusy || em.reviewNotice == "" || em.state != extractStateReview {
		t.Fatalf("after auth failure: busy=%v notice=%q state=%v", em.isSudoBusy, em.reviewNotice, em.state)
	}
	if len(drv.callsSnapshot()) != 0 {
		t.Fatal("Extract dispatched despite failed auth")
	}

	em, cmd, _ = dispatchKey(em, keys, "enter")
	probe = runCmd(t, cmd).(extractSudoProbeMsg)
	_ = em.applySudoProbe(probe)
	drv.push(extractResp{result: app.ExtractResult{}})
	if c := em.applySudoAuth(extractSudoAuthMsg{gen: em.gen, err: nil}); c == nil {
		t.Fatal("auth success must start the run")
	} else {
		runBatchLeaves(t, c)
	}
	if em.state != extractStateRunning {
		t.Fatalf("state = %v, want running after auth success", em.state)
	}
	calls := drv.callsSnapshot()
	if len(calls) != 1 || !calls[0].privileged {
		t.Fatalf("calls = %+v, want one privileged Extract", calls)
	}
}

// Leaving busy review supersedes in-flight probe and authentication results immediately.
func TestPrivilegedProbeAfterBackDoesNotStartRun(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	keys := defaultKeys()

	em, _, _ = dispatchKey(em, keys, "p")
	em, cmd, _ := dispatchKey(em, keys, "enter")
	probe, ok := runCmd(t, cmd).(extractSudoProbeMsg)
	if !ok {
		t.Fatalf("commit cmd produced %T, want extractSudoProbeMsg", probe)
	}
	staleGen := probe.gen

	em, backCmd, leave := dispatchKey(em, keys, "esc")
	if !leave || backCmd == nil {
		t.Fatalf("esc on busy review: leave=%v cmd=%v, want back-to-browse", leave, backCmd)
	}

	drv.push(extractResp{result: app.ExtractResult{}})
	if c := em.applySudoProbe(probe); c != nil {
		t.Fatal("stale probe after back still produced a command")
	}
	if c := em.applySudoAuth(extractSudoAuthMsg{gen: staleGen, err: nil}); c != nil {
		t.Fatal("stale auth success after back still produced a command")
	}
	if em.state == extractStateRunning {
		t.Fatal("stale probe/auth after back started the run")
	}
	if got := len(drv.callsSnapshot()); got != 0 {
		t.Fatalf("Extract dispatched after back: %d calls", got)
	}
}

// Unavailable privilege returns directly to review without authentication.
func TestPrivilegedCommitUnavailable(t *testing.T) {
	em, drv := newExtractFixture(t, dirReq())
	drv.probeErr = app.ErrPrivilegedExtractUnavailable
	keys := defaultKeys()

	em, _, _ = dispatchKey(em, keys, "p")
	em, cmd, _ := dispatchKey(em, keys, "enter")
	probe := runCmd(t, cmd).(extractSudoProbeMsg)
	if c := em.applySudoProbe(probe); c != nil {
		t.Fatal("unavailable runner must not return a follow-up Cmd")
	}
	if em.isSudoBusy || em.reviewNotice == "" || em.state != extractStateReview {
		t.Fatalf("busy=%v notice=%q state=%v, want idle review with notice", em.isSudoBusy, em.reviewNotice, em.state)
	}
	if len(drv.callsSnapshot()) != 0 {
		t.Fatal("Extract dispatched despite unavailable runner")
	}
}

// The review status slot distinguishes idle, busy, and failed privilege checks.
func TestExtractReviewBodySudoSlot(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	m := Model{styles: newStyles(theme.Default()), extract: em}

	if got := stripANSI(m.extractReviewBody(100)); strings.Contains(got, "checking sudo access") {
		t.Errorf("idle review shows the sudo-busy hint\n---\n%s", got)
	}

	m.extract.isSudoBusy = true
	if got := stripANSI(m.extractReviewBody(100)); !strings.Contains(got, "checking sudo access") {
		t.Errorf("busy review missing the sudo hint\n---\n%s", got)
	}

	m.extract.isSudoBusy = false
	m.extract.reviewNotice = "sudo authentication failed — cannot extract as root"
	if got := stripANSI(m.extractReviewBody(100)); !strings.Contains(got, "sudo authentication failed") {
		t.Errorf("review missing the failure notice\n---\n%s", got)
	}
}

// Review marks directory source/target rows and shows privilege state separately.
func TestExtractReviewBodyDirMarkers(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	m := Model{styles: newStyles(theme.Default()), extract: em}
	got := stripANSI(m.extractReviewBody(100))
	if !strings.Contains(got, "▸ "+dirReq().Source) {
		t.Errorf("dir review missing source marker\n---\n%s", got)
	}
	if strings.Count(got, "▸ ") != 2 {
		t.Errorf("dir review wants markers on Source and Target\n---\n%s", got)
	}
	if strings.Contains(got, "Output") {
		t.Errorf("review still renders the Output row\n---\n%s", got)
	}
	if strings.Contains(got, "ownership preserved") {
		t.Errorf("unprivileged review shows the as-root line\n---\n%s", got)
	}

	m.extract.req.Privileged = true
	if got := stripANSI(m.extractReviewBody(100)); !strings.Contains(got, "as root — snapshot file ownership preserved") {
		t.Errorf("privileged review missing the as-root line\n---\n%s", got)
	}

	fm, _ := newExtractFixture(t, fileReq())
	m = Model{styles: newStyles(theme.Default()), extract: fm}
	if got := stripANSI(m.extractReviewBody(100)); strings.Contains(got, "▸") {
		t.Errorf("file review must not carry dir markers\n---\n%s", got)
	}
}

// extractTargetValue includes the directory marker in its wrapping budget.
func TestExtractTargetValueWrapsOnlyWhenNeeded(t *testing.T) {
	const final = "/tmp/repo/a1b2c3d4/etc/nginx"
	if got := extractTargetValue(final, len(final), false); got != final {
		t.Errorf("fitting target wrapped: %q", got)
	}
	if got := extractTargetValue(final, len(final)+2, true); got != "▸ "+final {
		t.Errorf("fitting dir target wrapped: %q", got)
	}
	if got := extractTargetValue(final, len(final), true); got != "▸ /tmp/repo/a1b2c3d4/etc/ngi\nnx" {
		t.Errorf("marker must count against the fit budget, got %q", got)
	}
	if got := extractTargetValue(final, len(final)-1, false); got != "/tmp/repo/a1b2c3d4/etc/ngin\nx" {
		t.Errorf("overlong target must wrap, got %q", got)
	}
}

func TestPrivilegedStaleSudoMsgsDropped(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	em, _, _ = dispatchKey(em, keys, "p")
	em, _, _ = dispatchKey(em, keys, "enter")

	if c := em.applySudoProbe(extractSudoProbeMsg{gen: em.gen + 1, err: nil}); c != nil {
		t.Fatal("stale-gen probe msg must be dropped")
	}
	if em.state != extractStateReview {
		t.Fatalf("state changed on stale msg: %v", em.state)
	}
	if c := em.applySudoAuth(extractSudoAuthMsg{gen: em.gen + 1, err: nil}); c != nil {
		t.Fatal("stale-gen auth msg must be dropped")
	}
}

// extractTestSnapIDOlder is a second valid snapshot ID for detail fixtures.
const extractTestSnapIDOlder = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

// extractDetailModel selects a summarized newest snapshot and a summary-less older one.
func extractDetailModel(t *testing.T) Model {
	t.Helper()
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name:          "repo-a",
			RefreshedAt:   testNow,
			LastSnapshot:  testNow.Add(-time.Hour),
			SnapshotCount: 2,
			Snapshots: []model.Snapshot{
				{ID: extractTestSnapIDOlder, ShortID: extractTestSnapIDOlder[:8], Time: testNow.Add(-2 * time.Hour), Hostname: "h"},
				{
					ID: extractTestSnapID, ShortID: extractTestSnapID[:8], Time: testNow.Add(-time.Hour), Hostname: "h",
					Summary: &model.SnapshotSummary{TotalBytesProcessed: 4096},
				},
			},
		},
	})
	cfg, _ := extractCfg(t)
	a.Cfg.Extract = cfg
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("precondition: enter should open detailView, view = %d", m.view)
	}
	return m
}

// Whole-snapshot requests use root source and the short ID as source name.
func TestExtractRequestFromSnapshot(t *testing.T) {
	req, err := extractRequestFromSnapshot("repo-a", &model.Snapshot{ID: extractTestSnapID})
	if err != nil {
		t.Fatalf("extractRequestFromSnapshot: %v", err)
	}
	if req.Source != "/" {
		t.Errorf("Source = %q, want /", req.Source)
	}
	short := extractTestSnapID[:8]
	if req.SourceName != short || req.SnapshotShort != short {
		t.Errorf("SourceName/SnapshotShort = %q/%q, want %q", req.SourceName, req.SnapshotShort, short)
	}
	if req.SnapshotID != extractTestSnapID {
		t.Errorf("SnapshotID = %q, want %q", req.SnapshotID, extractTestSnapID)
	}
	if req.Mode != app.ExtractDirectoryTree {
		t.Errorf("Mode = %v, want ExtractDirectoryTree", req.Mode)
	}
	if req.WasRegularFile {
		t.Error("a whole snapshot must not be flagged WasRegularFile")
	}
	if req.TargetRoot != "" {
		t.Errorf("TargetRoot = %q, want empty (cfg default)", req.TargetRoot)
	}

	if _, err := extractRequestFromSnapshot("repo-a", nil); err == nil {
		t.Error("nil snapshot must be rejected")
	}
	if _, err := extractRequestFromSnapshot("repo-a", &model.Snapshot{ID: "ab12"}); err == nil {
		t.Error("a too-short id must be rejected")
	}
}

// Whole-snapshot output is the short-ID directory with repo-level staging.
func TestExtractRequestFromSnapshotPlansSnapshotDir(t *testing.T) {
	cfg, root := extractCfg(t)
	req, err := extractRequestFromSnapshot("repo-a", &model.Snapshot{ID: extractTestSnapID})
	if err != nil {
		t.Fatalf("extractRequestFromSnapshot: %v", err)
	}
	staging, final, err := app.PlanExtractPaths(cfg, req)
	if err != nil {
		t.Fatalf("PlanExtractPaths: %v", err)
	}
	if want := filepath.Join(root, "repo-a", extractTestSnapID[:8]); final != want {
		t.Errorf("final = %q, want the snapshot dir itself %q", final, want)
	}
	if filepath.Dir(staging) != filepath.Join(root, "repo-a") {
		t.Errorf("staging = %q, want a repo-level sibling of the snapshot dirs", staging)
	}
	if !strings.HasPrefix(filepath.Base(staging), ".resticscope-staging-") {
		t.Errorf("staging = %q, want the hidden staging prefix", staging)
	}
}

// Detail extraction targets the selected whole snapshot and returns to detail.
func TestDetailExtractKeyOpensSubModel(t *testing.T) {
	m := extractDetailModel(t)

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e on detail should open extractView, view = %d", m.view)
	}
	req := m.extract.req
	if req.Repo != "repo-a" {
		t.Errorf("Repo = %q, want repo-a", req.Repo)
	}
	if req.SnapshotID != extractTestSnapID {
		t.Errorf("SnapshotID = %q, want the newest snapshot %q", req.SnapshotID, extractTestSnapID)
	}
	if req.Source != "/" {
		t.Errorf("Source = %q, want /", req.Source)
	}
	if req.SourceName != extractTestSnapID[:8] {
		t.Errorf("SourceName = %q, want %q", req.SourceName, extractTestSnapID[:8])
	}
	if req.Mode != app.ExtractDirectoryTree {
		t.Errorf("Mode = %v, want ExtractDirectoryTree", req.Mode)
	}
	if m.extractReturn != detailView {
		t.Errorf("extractReturn = %d, want detailView", m.extractReturn)
	}
	if m.extract.srcSize != 4096 {
		t.Errorf("srcSize = %d, want 4096 (carried from Summary.TotalBytesProcessed)", m.extract.srcSize)
	}
}

// Summary-less legacy snapshots extract without a displayed source size.
func TestDetailExtractWithoutSummary(t *testing.T) {
	m := extractDetailModel(t)
	m = update(t, m, press("j"))

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e should open extractView, view = %d", m.view)
	}
	if m.extract.req.SnapshotID != extractTestSnapIDOlder {
		t.Errorf("SnapshotID = %q, want the older snapshot %q", m.extract.req.SnapshotID, extractTestSnapIDOlder)
	}
	if m.extract.srcSize != 0 {
		t.Errorf("srcSize = %d, want 0 for a summary-less snapshot", m.extract.srcSize)
	}
}

// Detail-launched extraction returns to intact detail state with the modal zeroed.
func TestDetailExtractReturnsToDetail(t *testing.T) {
	m := extractDetailModel(t)
	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("precondition: e should open extractView, view = %d", m.view)
	}

	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on review should return an exit cmd")
	}
	m = update(t, m, cmd())

	if m.view != detailView {
		t.Errorf("after leaving extract, view = %d, want detailView", m.view)
	}
	if m.extract.req.Source != "" || m.extract.staging != "" || m.extract.final != "" {
		t.Errorf("sub-model not zeroed on exit: %+v", m.extract.req)
	}
	if m.extractReturn != listView {
		t.Errorf("extractReturn = %d, want reset to the zero value", m.extractReturn)
	}
	if m.detailName != "repo-a" {
		t.Errorf("detailName = %q, want repo-a (detail context intact)", m.detailName)
	}
}

// Empty repositories show a selection hint without opening extraction.
func TestDetailExtractEmptyRepoStatusHint(t *testing.T) {
	a := testApp(map[string]model.RepoState{
		"repo-a": {Name: "repo-a", RefreshedAt: testNow},
	})
	cfg, _ := extractCfg(t)
	a.Cfg.Extract = cfg
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("precondition: enter should open detailView, view = %d", m.view)
	}

	m = update(t, m, press("e"))

	if m.view != detailView {
		t.Errorf("e with no snapshots should stay in detail, view = %d", m.view)
	}
	if m.statusMsg != "no snapshot selected" {
		t.Errorf("statusMsg = %q, want the no-selection hint", m.statusMsg)
	}
	if m.extract.req.Source != "" {
		t.Errorf("no sub-model should be built; req = %+v", m.extract.req)
	}
}

// Malformed cached snapshot IDs fail with a path-free notice in detail.
func TestDetailExtractMalformedIDStaysInDetail(t *testing.T) {
	a := detailApp(t)
	cfg, _ := extractCfg(t)
	a.Cfg.Extract = cfg
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))

	m = update(t, m, press("e"))

	if m.view != detailView {
		t.Errorf("e with a malformed id should stay in detail, view = %d", m.view)
	}
	if !strings.HasPrefix(m.statusMsg, "extract:") {
		t.Errorf("statusMsg = %q, want an extract error notice", m.statusMsg)
	}
	if strings.Contains(m.statusMsg, "id-") {
		t.Errorf("error notice leaked the snapshot id: %q", m.statusMsg)
	}
}

func TestDetailFooterAdvertisesExtract(t *testing.T) {
	m := extractDetailModel(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "e extract") {
		t.Errorf("detail footer should advertise 'e extract'\n---\n%s", footer)
	}
}

// The modal uses an origin-neutral back label.
func TestExtractFooterBackLabelOriginNeutral(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	keys := defaultKeys()
	for _, st := range []extractState{extractStateSuccess, extractStateError, extractStateKeepDelete} {
		em.state = st
		if got := footHelp(em, keys); strings.Contains(got, "back to browse") {
			t.Errorf("state %v footer = %q, want origin-neutral 'back'", st, got)
		}
	}
	em.state = extractStateSuccess
	if got := footHelp(em, keys); !strings.Contains(got, "q back") {
		t.Errorf("success footer = %q, want 'q back'", got)
	}
}

// extractFindVersionsModel isolates extraction across two version rows.
func extractFindVersionsModel(t *testing.T) Model {
	t.Helper()
	a := extractApp(t)
	m := newTestModel(t, a)
	m.view = findVersionsView
	m.findRepo = "repo-a"
	m.findPath = "/etc/debian_version"
	m.findRows = []model.FileVersion{
		{Size: 5, Occurrences: []model.FileVersionOccurrence{
			{SnapshotID: extractTestSnapID, ShortID: extractTestSnapID[:8]},
			{SnapshotID: extractTestSnapIDOlder, ShortID: extractTestSnapIDOlder[:8]},
		}},
		{Size: 7, Occurrences: []model.FileVersionOccurrence{
			{SnapshotID: extractTestSnapIDOlder, ShortID: extractTestSnapIDOlder[:8]},
		}},
	}
	return m
}

// Version extraction uses the selected row's newest occurrence and returns to versions.
func TestFindVersionsExtractKeyOpensSubModel(t *testing.T) {
	m := extractFindVersionsModel(t)

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e on a version row should open extractView, view = %d", m.view)
	}
	req := m.extract.req
	if req.Repo != "repo-a" {
		t.Errorf("Repo = %q, want repo-a", req.Repo)
	}
	if req.SnapshotID != extractTestSnapID {
		t.Errorf("SnapshotID = %q, want the newest occurrence %q", req.SnapshotID, extractTestSnapID)
	}
	if req.Source != "/etc/debian_version" {
		t.Errorf("Source = %q, want /etc/debian_version", req.Source)
	}
	if req.SourceName != "debian_version" {
		t.Errorf("SourceName = %q, want debian_version", req.SourceName)
	}
	if req.Mode != app.ExtractFile || !req.WasRegularFile {
		t.Errorf("Mode/WasRegularFile = %v/%v, want file extract", req.Mode, req.WasRegularFile)
	}
	if m.extractReturn != findVersionsView {
		t.Errorf("extractReturn = %d, want findVersionsView", m.extractReturn)
	}
	if m.extract.srcSize != 5 {
		t.Errorf("srcSize = %d, want 5 (the version row's size)", m.extract.srcSize)
	}
}

// Enter aliases extraction as the version row's primary action.
func TestFindVersionsEnterOpensSubModel(t *testing.T) {
	m := extractFindVersionsModel(t)

	m = update(t, m, press("enter"))

	if m.view != extractView {
		t.Fatalf("enter on a version row should open extractView, view = %d", m.view)
	}
	if m.extract.req.SnapshotID != extractTestSnapID {
		t.Errorf("SnapshotID = %q, want the newest occurrence %q", m.extract.req.SnapshotID, extractTestSnapID)
	}
	if m.extractReturn != findVersionsView {
		t.Errorf("extractReturn = %d, want findVersionsView", m.extractReturn)
	}
}

func TestFindVersionsEnterPausedWhileLoading(t *testing.T) {
	m := extractFindVersionsModel(t)
	m.findCancel = func() {}

	m = update(t, m, press("enter"))

	if m.view != findVersionsView {
		t.Errorf("enter while loading should stay in find-versions, view = %d", m.view)
	}
	if m.extract.req.Source != "" {
		t.Errorf("no sub-model should be built while loading; req = %+v", m.extract.req)
	}
}

// The cursor chooses which version occurrence is extracted.
func TestFindVersionsExtractUsesCursorRow(t *testing.T) {
	m := extractFindVersionsModel(t)
	m = update(t, m, press("j"))

	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("e should open extractView, view = %d", m.view)
	}
	if m.extract.req.SnapshotID != extractTestSnapIDOlder {
		t.Errorf("SnapshotID = %q, want the second row's occurrence %q", m.extract.req.SnapshotID, extractTestSnapIDOlder)
	}
	if m.extract.srcSize != 7 {
		t.Errorf("srcSize = %d, want 7", m.extract.srcSize)
	}
}

// Version-launched extraction returns to the intact version table.
func TestFindVersionsExtractReturnsToFindVersions(t *testing.T) {
	m := extractFindVersionsModel(t)
	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("precondition: e should open extractView, view = %d", m.view)
	}

	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on review should return an exit cmd")
	}
	m = update(t, m, cmd())

	if m.view != findVersionsView {
		t.Errorf("after leaving extract, view = %d, want findVersionsView", m.view)
	}
	if m.extract.req.Source != "" {
		t.Errorf("sub-model not zeroed on exit: %+v", m.extract.req)
	}
	if m.extractReturn != listView {
		t.Errorf("extractReturn = %d, want reset to the zero value", m.extractReturn)
	}
	if m.findPath != "/etc/debian_version" || len(m.findRows) != 2 {
		t.Errorf("find state should survive the modal round-trip: path=%q rows=%d", m.findPath, len(m.findRows))
	}
}

// Extraction without result rows is a no-op.
func TestFindVersionsExtractNoRowsIsNoop(t *testing.T) {
	m := extractFindVersionsModel(t)
	m.findRows = nil

	m = update(t, m, press("e"))

	if m.view != findVersionsView {
		t.Errorf("e with no rows should stay in find-versions, view = %d", m.view)
	}
	if m.extract.req.Source != "" {
		t.Errorf("no sub-model should be built; req = %+v", m.extract.req)
	}
}

// Loading suppresses extraction because rows are pending replacement.
func TestFindVersionsExtractPausedWhileLoading(t *testing.T) {
	m := extractFindVersionsModel(t)
	m.findCancel = func() {}

	m = update(t, m, press("e"))

	if m.view != findVersionsView {
		t.Errorf("e while loading should stay in find-versions, view = %d", m.view)
	}
	if m.extract.req.Source != "" {
		t.Errorf("no sub-model should be built while loading; req = %+v", m.extract.req)
	}
}

func TestFindVersionsFooterAdvertisesExtract(t *testing.T) {
	m := extractFindVersionsModel(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "enter extract") {
		t.Errorf("find-versions footer should advertise 'enter extract'\n---\n%s", footer)
	}
}

// Review shows known indexed descendant counts and hides unknown counts.
func TestExtractReviewContainsRow(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	em.drv = &fakeExtractDriver{subFiles: 12, subDirs: 3, subKnown: true}

	cmd := em.countsCmd()
	if cmd == nil {
		t.Fatal("countsCmd returned nil for a directory source")
	}
	msg, ok := cmd().(extractCountsMsg)
	if !ok {
		t.Fatalf("countsCmd message = %T, want extractCountsMsg", cmd())
	}
	em.applyCounts(msg)

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
	m.extract = em
	body := stripANSI(m.extractBody())
	if !strings.Contains(body, "Contains") || !strings.Contains(body, "12 files · 3 dirs") {
		t.Errorf("review body missing the Contains row:\n%s", body)
	}
}

// Files skip count lookup, while stale and unknown results are discarded.
func TestExtractReviewContainsRowGuards(t *testing.T) {
	a := extractApp(t)
	fm, err := newExtractModel(a, t.Context(), fileReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	fm.drv = &fakeExtractDriver{subKnown: true}
	if cmd := fm.countsCmd(); cmd != nil {
		t.Error("countsCmd should be nil for a file source")
	}

	dm, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	dm.applyCounts(extractCountsMsg{gen: dm.gen + 1, files: 9, dirs: 9, known: true})
	if dm.srcCountsKnown {
		t.Error("stale-generation counts must be dropped")
	}
	dm.applyCounts(extractCountsMsg{gen: dm.gen, known: false})
	if dm.srcCountsKnown {
		t.Error("known=false counts must be dropped")
	}
}

// Review surfaces the same occupied-target state enforced at run time.
func TestExtractReviewTargetExistsNote(t *testing.T) {
	a := extractApp(t)
	em, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	if em.isTargetBusy {
		t.Fatal("fresh target plan must not start busy")
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
	m.extract = em
	if body := stripANSI(m.extractBody()); strings.Contains(body, "target already exists") {
		t.Errorf("fresh-target review body carries the occupancy note:\n%s", body)
	}

	if err := os.MkdirAll(em.final, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	em2, err := newExtractModel(a, t.Context(), dirReq(), 0)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	if !em2.isTargetBusy {
		t.Fatal("occupied final must set isTargetBusy")
	}
	m.extract = em2
	if body := stripANSI(m.extractBody()); !strings.Contains(body, "target already exists — choose another target or remove the existing output") {
		t.Errorf("review body missing the occupancy note:\n%s", body)
	}
}

// Detail extraction dispatches the whole-snapshot count lookup.
func TestDetailExtractFiresCountsLookup(t *testing.T) {
	m := extractDetailModel(t)
	next, cmd := m.Update(press("e"))
	m = next.(Model)
	if m.view != extractView {
		t.Fatalf("e on detail should open extractView, view = %d", m.view)
	}
	if cmd == nil {
		t.Fatal("opening a directory extract should fire the counts lookup")
	}
	if _, ok := cmd().(extractCountsMsg); !ok {
		t.Fatalf("opener cmd message = %T, want extractCountsMsg", cmd())
	}
}

// Free-space warnings are advisory and require both known size and probe result.
func TestExtractReviewSpaceWarning(t *testing.T) {
	a := extractApp(t)
	// An impossible source size reliably exceeds the test filesystem.
	em, err := newExtractModel(a, t.Context(), dirReq(), 1<<62)
	if err != nil {
		t.Fatalf("newExtractModel: %v", err)
	}
	if !em.targetFreeKnown {
		t.Fatal("space probe must answer on a real filesystem")
	}

	m := newTestModel(t, a)
	m.view = extractView
	m.width = 240
	m.extract = em
	body := stripANSI(m.extractBody())
	if !strings.Contains(body, "source may not fit the target filesystem") || !strings.Contains(body, "needed") {
		t.Errorf("review body missing the space warning:\n%s", body)
	}

	em.srcSize = 0
	m.extract = em
	if body := stripANSI(m.extractBody()); strings.Contains(body, "may not fit") {
		t.Errorf("size-unknown review body carries the space warning:\n%s", body)
	}

	em.srcSize = 1 << 62
	em.targetFreeKnown = false
	m.extract = em
	if body := stripANSI(m.extractBody()); strings.Contains(body, "may not fit") {
		t.Errorf("probe-less review body carries the space warning:\n%s", body)
	}
}

// startRun records explicit dispatched targets, including cancelled runs.
func TestExtractStartRunRecordsTargetRoot(t *testing.T) {
	keys := defaultKeys()
	em, _ := newExtractFixture(t, dirReq())
	em, _, _ = dispatchKey(em, keys, "enter")
	if em.state != extractStateRunning {
		t.Fatalf("enter on review should start the run, state = %v", em.state)
	}
	if em.ranTargetRoot != "" {
		t.Errorf("ranTargetRoot = %q after a config-default run, want empty", em.ranTargetRoot)
	}

	em2, _ := newExtractFixture(t, dirReq())
	picked := t.TempDir()
	req, staging, final, err := planExtractOverride(em2.cfg, em2.req, picked)
	if err != nil {
		t.Fatalf("planExtractOverride: %v", err)
	}
	em2.req, em2.staging, em2.final = req, staging, final
	em2, _, _ = dispatchKey(em2, keys, "enter")
	if em2.ranTargetRoot != picked {
		t.Errorf("ranTargetRoot = %q, want the overridden root %q", em2.ranTargetRoot, picked)
	}
}

// Session target memory keeps dispatched roots and forgets picker-only choices.
func TestExtractTargetMemoRemembersLastRunTarget(t *testing.T) {
	m := extractDetailModel(t)
	picked := t.TempDir()

	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("precondition: e should open extractView, view = %d", m.view)
	}
	m.extract.req.TargetRoot = picked
	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on review should return an exit cmd")
	}
	m = update(t, m, cmd())
	if m.extractTargetMemo != "" {
		t.Fatalf("extractTargetMemo = %q after a run-less retarget, want empty", m.extractTargetMemo)
	}

	// Dispatch qualifies the target even when the worker is not run and is cancelled.
	m = update(t, m, press("e"))
	m.extract.req.TargetRoot = picked
	next, _ = m.Update(press("enter"))
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("enter on review should start the run, state = %v", m.extract.state)
	}
	m = update(t, m, extractRunDoneMsg{gen: m.extract.gen, err: context.Canceled})
	if m.extract.state != extractStateCanceled {
		t.Fatalf("state = %v after a cancelled run, want canceled", m.extract.state)
	}
	next, cmd = m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on the canceled screen should return an exit cmd")
	}
	m = update(t, m, cmd())
	if m.extractTargetMemo != picked {
		t.Fatalf("extractTargetMemo = %q, want the run target %q", m.extractTargetMemo, picked)
	}

	m = update(t, m, press("e"))
	if m.extract.req.TargetRoot != picked {
		t.Errorf("reopened req.TargetRoot = %q, want the remembered %q", m.extract.req.TargetRoot, picked)
	}
	if !strings.HasPrefix(m.extract.final, picked+string(filepath.Separator)) {
		t.Errorf("reopened final = %q, want it under %q", m.extract.final, picked)
	}
}

// Target picking starts from the request's effective override.
func TestExtractFilePickerStartsAtOverrideRoot(t *testing.T) {
	em, _ := newExtractFixture(t, dirReq())
	picked := t.TempDir()
	em.req.TargetRoot = picked
	em.height = 24
	_ = em.ensureFilepicker()
	if em.filepicker.CurrentDirectory != picked {
		t.Errorf("picker dir = %q, want the override root %q", em.filepicker.CurrentDirectory, picked)
	}
}

// remember_target=false disables memo capture and seeding.
func TestExtractTargetMemoDisabled(t *testing.T) {
	m := extractDetailModel(t)
	m.app.Cfg.Extract.RememberTarget = false
	picked := t.TempDir()

	m = update(t, m, press("e"))
	m.extract.req.TargetRoot = picked
	next, _ := m.Update(press("enter"))
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("enter on review should start the run, state = %v", m.extract.state)
	}
	m = update(t, m, extractRunDoneMsg{gen: m.extract.gen, err: context.Canceled})
	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("esc on the canceled screen should return an exit cmd")
	}
	m = update(t, m, cmd())
	if m.extractTargetMemo != "" {
		t.Fatalf("extractTargetMemo = %q with remember_target = false, want empty", m.extractTargetMemo)
	}
	m = update(t, m, press("e"))
	if m.extract.req.TargetRoot != "" {
		t.Errorf("reopened req.TargetRoot = %q with remember_target = false, want empty", m.extract.req.TargetRoot)
	}
}
