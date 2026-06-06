package resticx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"resticscope/internal/model"
)

// extractBytesStreamFake feeds canned stdout bytes to onStdout, records the
// argv/env, counts how many times restic was "spawned" (started), and can
// deliver a slow stream (one chunk, then block on ctx) so a mid-stream cancel is
// deterministic. It mirrors extractTreeStreamFake.
type extractBytesStreamFake struct {
	data       []byte
	stderr     []byte
	err        error
	slow       bool
	failReader io.Reader // when set, fed to onStdout to force a copy error
	started    int
	gotArgs    []string
	gotEnv     []string
	// cbCtxCanceled records whether the (child) context was already canceled when
	// onStdout returned — i.e. whether the wrapper canceled restic before this
	// runner would have drained the rest of stdout and waited.
	cbCtxCanceled bool
}

func (f *extractBytesStreamFake) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) ([]byte, error) {
	f.started++
	f.gotEnv = env
	f.gotArgs = args
	if f.failReader != nil {
		// Production RunStream drains remaining stdout then Waits after the callback;
		// with the child ctx canceled both finish promptly. We only need to observe
		// whether the wrapper canceled before returning, which is what spares a long
		// remote read on a local copy failure.
		cbErr := onStdout(f.failReader)
		f.cbCtxCanceled = ctx.Err() != nil
		return f.stderr, cbErr
	}
	if f.slow {
		// Deliver the chunk once; the next read blocks until ctx is canceled,
		// emulating exec.CommandContext killing restic and the stdout pipe closing.
		// Reuses browse_test.go's blockingReader. ExtractBytes keys cancellation off
		// ctx.Err(), so the reader's EOF-after-cancel still yields context.Canceled.
		cbErr := onStdout(&blockingReader{data: string(f.data), ctx: ctx})
		return f.stderr, cbErr
	}
	cbErr := onStdout(bytes.NewReader(f.data))
	if cbErr != nil {
		return f.stderr, cbErr
	}
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
}

// errReader yields its data once (with the error attached) so io.Copy writes the
// bytes and then unwinds with err — emulating a local write failure partway
// through the stream.
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

// --- argv tests (the safety-invariant core) ---------------------------------

func TestBuildExtractBytesArgs(t *testing.T) {
	p := ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: "/abs/staging/hosts"}
	got, err := buildExtractBytesArgs(p)
	if err != nil {
		t.Fatalf("buildExtractBytesArgs: %v", err)
	}
	want := []string{"--no-lock", "dump", testSnapID, "/etc/hosts"}
	if !slices.Equal(got, want) {
		t.Errorf("args =\n  %q\nwant\n  %q", got, want)
	}
	// Invariants restated as standalone assertions.
	assertNoForbiddenArgs(t, got)
	if !slices.Contains(got, "--no-lock") {
		t.Error("argv must always carry --no-lock")
	}
	// resticscope owns the output file: dump's target-writing / overwrite / dry-run
	// flags must never appear.
	for _, forbidden := range []string{"--target", "--overwrite", "--dry-run"} {
		if slices.Contains(got, forbidden) {
			t.Errorf("bytes argv must not contain %q: %q", forbidden, got)
		}
	}
	// The snapshot reference is the verified hex ID, never "latest".
	if got[2] != testSnapID {
		t.Errorf("snapshot argv element = %q, want %q", got[2], testSnapID)
	}
}

func TestBuildExtractBytesArgsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		p       ExtractBytesParams
		wantErr error
	}{
		{"parent-escape source", ExtractBytesParams{SnapshotID: testSnapID, Source: "../oops", Target: "/abs/x"}, ErrExtractInvalidSource},
		{"unrooted source", ExtractBytesParams{SnapshotID: testSnapID, Source: "etc/hosts", Target: "/abs/x"}, ErrExtractInvalidSource},
		{"uncleaned dotdot source", ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/../hosts", Target: "/abs/x"}, ErrExtractInvalidSource},
		// Unlike tree mode, the whole-snapshot root is not a dumpable file.
		{"empty source", ExtractBytesParams{SnapshotID: testSnapID, Source: "", Target: "/abs/x"}, ErrExtractInvalidSource},
		{"root source", ExtractBytesParams{SnapshotID: testSnapID, Source: "/", Target: "/abs/x"}, ErrExtractInvalidSource},
		{"NUL in source", ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/\x00sts", Target: "/abs/x"}, ErrExtractInvalidSource},
		{"non-hex snapshot", ExtractBytesParams{SnapshotID: "not-hex!", Source: "/etc/hosts", Target: "/abs/x"}, ErrExtractInvalidSnapshotID},
		{"latest snapshot", ExtractBytesParams{SnapshotID: "latest", Source: "/etc/hosts", Target: "/abs/x"}, ErrExtractInvalidSnapshotID},
		// A short ID is a valid hex prefix but must be rejected: restic would
		// resolve it as an ambiguous prefix (00-framework.md §5).
		{"short hex ID", ExtractBytesParams{SnapshotID: testSnapID[:8], Source: "/etc/hosts", Target: "/abs/x"}, ErrExtractInvalidSnapshotID},
		{"over-length hex ID", ExtractBytesParams{SnapshotID: testSnapID + "ab", Source: "/etc/hosts", Target: "/abs/x"}, ErrExtractInvalidSnapshotID},
		{"empty target", ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: ""}, ErrExtractInvalidTarget},
		{"relative target", ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: "rel/path"}, ErrExtractInvalidTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildExtractBytesArgs(tt.p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("argv must not be produced on rejection, got %q", got)
			}
		})
	}
}

// TestBuildExtractBytesArgsNoShellInterpolation pins that a previously-cleaned
// source carrying shell metacharacters survives as the exact literal positional
// argv element — no expansion, quoting, escaping, or splitting. The args are an
// []string handed straight to exec, so no shell ever sees them.
func TestBuildExtractBytesArgsNoShellInterpolation(t *testing.T) {
	base, err := buildExtractBytesArgs(ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/plain", Target: "/abs/x"})
	if err != nil {
		t.Fatalf("baseline buildExtractBytesArgs: %v", err)
	}
	for _, src := range []string{"/etc/$(whoami)", "/etc/foo;rm -rf .;bar", "/etc/`id`", "/etc/a|b&c"} {
		t.Run(src, func(t *testing.T) {
			if src != model.CleanBrowsePath(src) {
				t.Fatalf("test input %q is not already clean (CleanBrowsePath = %q)", src, model.CleanBrowsePath(src))
			}
			got, err := buildExtractBytesArgs(ExtractBytesParams{SnapshotID: testSnapID, Source: src, Target: "/abs/x"})
			if err != nil {
				t.Fatalf("buildExtractBytesArgs: %v", err)
			}
			// dump argv is [--no-lock, dump, <snap>, <source>]: source is index 3.
			if got[3] != src {
				t.Errorf("source argv element = %q, want exact literal %q", got[3], src)
			}
			if len(got) != len(base) {
				t.Errorf("argv length = %d, want %d (metacharacters must add no tokens)", len(got), len(base))
			}
			assertNoForbiddenArgs(t, got)
		})
	}
}

// --- copy / O_EXCL semantics ------------------------------------------------

func TestExtractBytesCopiesStreamToTarget(t *testing.T) {
	fixture := readFixture(t, "restic-0.18-dump.bin")
	fs := &extractBytesStreamFake{data: fixture}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "out.bin")

	var progressCalls int
	var lastBytes int64
	res, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target},
		func(p ExtractBytesProgress) { progressCalls++; lastBytes = p.BytesDone })
	if err != nil {
		t.Fatalf("ExtractBytes: %v", err)
	}
	if res.BytesWritten != int64(len(fixture)) {
		t.Errorf("BytesWritten = %d, want %d", res.BytesWritten, len(fixture))
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if !bytes.Equal(got, fixture) {
		t.Error("target bytes differ from the fixture (raw copy must be byte-exact)")
	}
	if progressCalls < 1 {
		t.Error("expected at least one onProgress call")
	}
	if lastBytes != int64(len(fixture)) {
		t.Errorf("final progress BytesDone = %d, want %d", lastBytes, len(fixture))
	}
	if fs.started != 1 {
		t.Errorf("restic spawned %d times, want 1", fs.started)
	}
}

func TestExtractBytesRefusesExistingTarget(t *testing.T) {
	fs := &extractBytesStreamFake{data: []byte("should-not-be-used")}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatalf("pre-create target: %v", err)
	}

	_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil)
	if !errors.Is(err, ErrExtractTargetExists) {
		t.Fatalf("err = %v, want ErrExtractTargetExists", err)
	}
	if fs.started != 0 {
		t.Errorf("restic must not be spawned when the target exists (started = %d)", fs.started)
	}
	// The pre-existing file must be left untouched (O_EXCL never opened it).
	if got, _ := os.ReadFile(target); string(got) != "existing" {
		t.Errorf("existing target was modified: %q", got)
	}
	// The sentinel must not echo the target path.
	if strings.Contains(err.Error(), target) || strings.Contains(err.Error(), "out.bin") {
		t.Errorf("error leaked the target path: %q", err.Error())
	}
}

// TestExtractBytesCreateFailureIsPathFree drives a non-EEXIST open failure (a
// missing parent dir, uid-independent) and asserts it is neither
// ErrExtractTargetExists nor a path leak, and that restic was never spawned.
func TestExtractBytesCreateFailureIsPathFree(t *testing.T) {
	fs := &extractBytesStreamFake{data: []byte("x")}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "missing-parent", "out.bin")

	_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil)
	if err == nil {
		t.Fatal("expected an error creating the target under a missing parent")
	}
	if errors.Is(err, ErrExtractTargetExists) {
		t.Error("a missing-parent failure must not map to ErrExtractTargetExists")
	}
	if fs.started != 0 {
		t.Errorf("restic must not be spawned on a create failure (started = %d)", fs.started)
	}
	for _, leak := range []string{target, "missing-parent", "out.bin"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaked the target path fragment %q: %q", leak, err.Error())
		}
	}
}

// TestExtractBytesMidStreamCancel cancels after the first chunk and asserts the
// error is context.Canceled, the partial file remains on disk (this layer never
// deletes it), and BytesWritten matches the bytes copied before cancel.
func TestExtractBytesMidStreamCancel(t *testing.T) {
	const chunk = "first-chunk-of-bytes"
	fs := &extractBytesStreamFake{slow: true, data: []byte(chunk)}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "out.bin")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	res, err := c.ExtractBytes(ctx, testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target},
		func(p ExtractBytesProgress) { calls++; cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.BytesWritten != int64(len(chunk)) {
		t.Errorf("BytesWritten = %d, want %d", res.BytesWritten, len(chunk))
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("partial file must remain on disk after cancel: %v", readErr)
	}
	if string(got) != chunk {
		t.Errorf("partial file = %q, want the first chunk %q", got, chunk)
	}
}

// TestExtractBytesCopyErrorCancelsRestic pins that a local copy failure cancels
// the child context before RunStream returns, so the production runner does not
// drain the remainder of a large dump's stdout before waiting. The error stays a
// path-free wrapper around the copy failure.
func TestExtractBytesCopyErrorCancelsRestic(t *testing.T) {
	boom := errors.New("disk write boom")
	fs := &extractBytesStreamFake{failReader: &errReader{data: []byte("partial"), err: boom}}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "out.bin")

	_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil)
	if err == nil {
		t.Fatal("expected a copy failure error")
	}
	if errors.Is(err, ErrExtractTargetExists) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a wrapped local copy failure", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err should wrap the copy failure %v, got %v", boom, err)
	}
	if !fs.cbCtxCanceled {
		t.Error("ExtractBytes must cancel the child context on a local copy error")
	}
	for _, leak := range []string{target, "out.bin"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaked the target path %q: %q", leak, err.Error())
		}
	}
}

// --- exit-code classification -----------------------------------------------

func TestExtractBytesClassifiesExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		exit   fakeExit
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExit(10), "repository does not exist", KindRepoNotFound},
		{"wrong password", fakeExit(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"generic failure", fakeExit(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &extractBytesStreamFake{err: tt.exit, stderr: []byte(tt.stderr)}
			c := &Client{Stream: fs}
			target := filepath.Join(t.TempDir(), "out.bin")
			_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
				ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil)
			var re *Error
			if !asResticError(err, &re) {
				t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
			}
			if re.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", re.Kind, tt.want)
			}
			if re.Op != "dump" {
				t.Errorf("Op = %q, want dump", re.Op)
			}
		})
	}
}

// --- stderr sanitization (privacy) ------------------------------------------

func TestExtractBytesScrubsPathsKeepsSecretMask(t *testing.T) {
	const source = "/etc/secret-file"
	target := filepath.Join(t.TempDir(), "out.bin")
	fs := &extractBytesStreamFake{
		err:    fakeExit(1),
		stderr: []byte("Fatal: AK-LEAK-123 failed dumping " + source + " boom"),
	}
	c := &Client{
		Stream: fs,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAK-123", "[REDACTED]") },
	}
	_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: source, Target: target}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("expected secret mask in error, got %q", msg)
	}
	for _, leak := range []string{"AK-LEAK-123", source, "secret-file"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaked %q: %q", leak, msg)
		}
	}
}

func TestExtractBytesDropsPathHeavyStderr(t *testing.T) {
	fs := &extractBytesStreamFake{
		err:    fakeExit(1),
		stderr: []byte("Fatal: error reading /var/lib/other/unrelated/file: permission denied"),
	}
	c := &Client{Stream: fs}
	target := filepath.Join(t.TempDir(), "out.bin")
	_, err := c.ExtractBytes(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil)
	var re *Error
	if !asResticError(err, &re) {
		t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
	}
	if re.Kind != KindUnknown {
		t.Errorf("Kind = %v, want KindUnknown (classification preserved)", re.Kind)
	}
	if re.Stderr != "" {
		t.Errorf("path-heavy stderr must be dropped, got %q", re.Stderr)
	}
	if strings.Contains(err.Error(), "/var/lib/other/unrelated/file") {
		t.Errorf("error leaked an un-enumerated path: %q", err.Error())
	}
}

// --- process construction ---------------------------------------------------

func TestExtractBytesPasswordOutOfBandAndBucketLookup(t *testing.T) {
	fs := &extractBytesStreamFake{}
	c := &Client{Stream: fs}
	tgt := testTarget
	tgt.BucketLookup = "dns"
	target := filepath.Join(t.TempDir(), "out.bin")
	if _, err := c.ExtractBytes(context.Background(), tgt,
		Creds{AccessKey: "AK", SecretKey: "SK", ResticPassword: "super-secret-pw"},
		ExtractBytesParams{SnapshotID: testSnapID, Source: "/etc/hosts", Target: target}, nil); err != nil {
		t.Fatalf("ExtractBytes: %v", err)
	}
	env := strings.Join(fs.gotEnv, "\n")
	if strings.Contains(env, "super-secret-pw") {
		t.Error("password leaked into env")
	}
	if !strings.Contains(env, "RESTIC_PASSWORD_FILE=/dev/fd/3") {
		t.Error("env should reference the password file on fd 3")
	}
	if strings.Contains(strings.Join(fs.gotArgs, " "), "super-secret-pw") {
		t.Error("password leaked into argv")
	}
	// The bucket-lookup option is prepended by ExtractBytes (not buildExtractBytesArgs).
	if !slices.Equal(fs.gotArgs[:2], []string{"-o", "s3.bucket-lookup=dns"}) {
		t.Errorf("argv must prepend the bucket-lookup option, got %q", fs.gotArgs)
	}
	if !slices.Equal(fs.gotArgs[2:], []string{"--no-lock", "dump", testSnapID, "/etc/hosts"}) {
		t.Errorf("dump argv after bucket-lookup = %q", fs.gotArgs[2:])
	}
	assertNoForbiddenArgs(t, fs.gotArgs)
}
