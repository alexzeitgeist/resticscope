package app

// extract_privileged_test.go covers the parent (non-root) side of a privileged
// extract: routing through App.Extract, the stdin payload contract, replaying
// the helper's NDJSON events into result/onProgress, and reconstructing the
// sentinel errors across the process boundary. The runner is faked — the real
// sudo launch is exercised by the manual test note in
// docs/extract-privileged.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePrivRunner replays scripted stdout lines and records the payload.
type fakePrivRunner struct {
	mu       sync.Mutex
	probeErr error
	lines    []string
	runErr   error
	payloads [][]byte
}

func (f *fakePrivRunner) Probe(ctx context.Context) error { return f.probeErr }

func (f *fakePrivRunner) AuthCommand() *exec.Cmd { return exec.Command("true") }

func (f *fakePrivRunner) Run(ctx context.Context, payload []byte, onLine func([]byte) error) error {
	f.mu.Lock()
	f.payloads = append(f.payloads, payload)
	f.mu.Unlock()
	for _, l := range f.lines {
		if err := onLine([]byte(l)); err != nil {
			return err
		}
	}
	return f.runErr
}

func mustEventLine(t *testing.T, ev helperEvent) string {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func privReq() ExtractRequest {
	req := treeReq()
	req.Privileged = true
	return req
}

func TestExtractPrivilegedHappyPath(t *testing.T) {
	root := t.TempDir()
	a, fc := newExtractApp(fakeRestic{}, root)
	want := ExtractResult{Files: 3, Dirs: 1, Bytes: 42, FinalDir: root + "/repo-a/abcd1234/etc/nginx", FinalPath: root + "/repo-a/abcd1234/etc/nginx"}
	runner := &fakePrivRunner{lines: []string{
		mustEventLine(t, helperEvent{Kind: helperEventProgress, Progress: &ExtractProgress{BytesDone: 21, BytesTotal: 42}}),
		mustEventLine(t, helperEvent{Kind: helperEventResult, Result: &want}),
	}}
	a.Priv = runner

	var progress []ExtractProgress
	res, err := a.Extract(context.Background(), privReq(), func(p ExtractProgress) { progress = append(progress, p) })
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != want.Files || res.Dirs != want.Dirs || res.Bytes != want.Bytes || res.FinalPath != want.FinalPath {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if len(progress) != 1 || progress[0].BytesDone != 21 {
		t.Errorf("progress = %+v, want one tick with BytesDone=21", progress)
	}
	if len(fc.saved) != 0 {
		t.Errorf("privileged extract persisted cache state: %v", fc.saved)
	}

	// The payload must carry the resolved creds, the wire version, and an
	// explicit absolute target root.
	var payload helperPayload
	if err := json.Unmarshal(runner.payloads[0], &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Version != helperPayloadVersion {
		t.Errorf("payload version = %d, want %d", payload.Version, helperPayloadVersion)
	}
	if payload.Creds.ResticPassword != "pw" || payload.Creds.AccessKey != "AK" {
		t.Errorf("payload creds not the resolved material: %+v", payload.Creds)
	}
	if payload.Request.TargetRoot != root {
		t.Errorf("payload target root = %q, want %q", payload.Request.TargetRoot, root)
	}
	if payload.TimeoutSeconds != int64((2 * time.Minute).Seconds()) {
		t.Errorf("payload timeout = %d, want 120", payload.TimeoutSeconds)
	}
}

func TestExtractPrivilegedSentinelRoundTrip(t *testing.T) {
	tests := []struct {
		code string
		want error
	}{
		{helperCodeStagingExists, ErrExtractStagingExists},
		{helperCodeFinalExists, ErrExtractFinalExists},
		{helperCodeCanceled, context.Canceled},
		{helperCodeDeadline, context.DeadlineExceeded},
		{helperCodeInvalidRequest, ErrExtractInvalidRequest},
		{helperCodeMetadataNorm, ErrExtractMetadataNormalization},
		{helperCodeRenameFailed, ErrExtractRenameFailed},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			a, _ := newExtractApp(fakeRestic{}, t.TempDir())
			a.Priv = &fakePrivRunner{lines: []string{
				mustEventLine(t, helperEvent{Kind: helperEventError, Error: &helperError{
					Code: tt.code, Message: "extract: refused", StagingDir: "/x/staging", StagingCreated: true,
				}}),
			}}
			res, err := a.Extract(context.Background(), privReq(), nil)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want errors.Is(%v)", err, tt.want)
			}
			if !res.StagingCreated || res.StagingDir != "/x/staging" {
				t.Errorf("staging fate not surfaced: %+v", res)
			}
		})
	}
}

func TestExtractPrivilegedUnavailableWithoutRunner(t *testing.T) {
	a, _ := newExtractApp(fakeRestic{}, t.TempDir())
	if _, err := a.Extract(context.Background(), privReq(), nil); !errors.Is(err, ErrPrivilegedExtractUnavailable) {
		t.Fatalf("err = %v, want ErrPrivilegedExtractUnavailable", err)
	}
	if err := a.PrivilegedExtractProbe(context.Background()); !errors.Is(err, ErrPrivilegedExtractUnavailable) {
		t.Fatalf("probe err = %v, want ErrPrivilegedExtractUnavailable", err)
	}
}

func TestExtractPrivilegedRunnerFailureWithoutEvents(t *testing.T) {
	a, _ := newExtractApp(fakeRestic{}, t.TempDir())
	a.Priv = &fakePrivRunner{runErr: errors.New("sudo: a password is required")}
	res, err := a.Extract(context.Background(), privReq(), nil)
	if err == nil || !strings.Contains(err.Error(), "helper") {
		t.Fatalf("err = %v, want a helper failure", err)
	}
	// No staging dir exists (the helper never ran), so none may be reported.
	if res.StagingCreated {
		t.Errorf("StagingCreated = true with no staging on disk")
	}
}

func TestExtractPrivilegedValidatesBeforeLaunch(t *testing.T) {
	a, _ := newExtractApp(fakeRestic{}, t.TempDir())
	runner := &fakePrivRunner{}
	a.Priv = runner
	req := privReq()
	req.SourceName = "wrong-slug"
	if _, err := a.Extract(context.Background(), req, nil); !errors.Is(err, ErrExtractInvalidRequest) {
		t.Fatalf("err = %v, want ErrExtractInvalidRequest", err)
	}
	if len(runner.payloads) != 0 {
		t.Error("invalid request still reached the helper runner")
	}
}

// TestHelperWireSentinelRoundTrip drives every classifiable pipeline error
// through the full wire contract — helperErrCode on the helper side, then
// helperSentinelError on the parent side — and asserts both the errors.Is
// identity and any cause detail survive the process boundary.
func TestHelperWireSentinelRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   error
		detail string
	}{
		{"canceled", fmt.Errorf("extract: %w", context.Canceled), context.Canceled, ""},
		{"deadline", fmt.Errorf("extract: %w", context.DeadlineExceeded), context.DeadlineExceeded, ""},
		{"staging_exists", ErrExtractStagingExists, ErrExtractStagingExists, ""},
		{"final_exists", ErrExtractFinalExists, ErrExtractFinalExists, ""},
		{"invalid_request", invalidExtractRequest("source_name"), ErrExtractInvalidRequest, "source_name"},
		{"metadata_normalization", fmt.Errorf("%w: %v", ErrExtractMetadataNormalization, "operation not permitted"), ErrExtractMetadataNormalization, "operation not permitted"},
		{"rename_failed", fmt.Errorf("%w: %v", ErrExtractRenameFailed, "cross-device link"), ErrExtractRenameFailed, "cross-device link"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := helperSentinelError(helperErrCode(tt.err), tt.err.Error())
			if !errors.Is(got, tt.want) {
				t.Fatalf("errors.Is identity lost across the wire: %v -> %v", tt.err, got)
			}
			if tt.detail != "" && !strings.Contains(got.Error(), tt.detail) {
				t.Errorf("cause detail lost across the wire: %q does not contain %q", got.Error(), tt.detail)
			}
		})
	}
}

func TestHelperSentinelErrorUnknownCodeKeepsMessage(t *testing.T) {
	err := helperSentinelError(helperCodeGeneric, "extract: staging metadata normalization failed: boom")
	if err == nil || !strings.Contains(err.Error(), "normalization") {
		t.Fatalf("err = %v, want the helper message verbatim", err)
	}
}
