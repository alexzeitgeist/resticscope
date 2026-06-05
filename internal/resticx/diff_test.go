package resticx

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"resticscope/internal/model"
)

// diffStreamFake feeds canned NDJSON to onStdout once, optionally returning a
// run error to exercise the classify path.
type diffStreamFake struct {
	data    string
	stderr  []byte
	err     error
	block   bool
	gotArgs []string
	gotEnv  []string
}

func (f *diffStreamFake) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) ([]byte, error) {
	f.gotEnv = env
	f.gotArgs = args
	if f.block {
		<-ctx.Done()
		return f.stderr, ctx.Err()
	}
	cbErr := onStdout(strings.NewReader(f.data))
	if cbErr != nil {
		return f.stderr, cbErr
	}
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
}

// TestStreamDiffParsesFixture is the diff-side golden test, matching the
// pattern of TestSnapshotsParsesFixture for snapshots. The fixture is a real
// restic 0.18 capture (one NDJSON record per change plus a terminal
// `statistics` envelope) and locks the parser against any drift in the
// message shape — field names, type semantics, the envelope being on its own
// line — that would otherwise only surface against a live repo.
//
// The current capture only exercises `+` and `M` modifiers; recapturing
// between snapshots that include a delete (`-`), a `chmod`-only change (`U`),
// or a file→symlink swap (`T`) would tighten the coverage further.
func TestStreamDiffParsesFixture(t *testing.T) {
	fs := &diffStreamFake{data: string(readFixture(t, "restic-0.18-diff.ndjson"))}
	c := &Client{Stream: fs}

	var entries []model.DiffEntry
	res, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		"aa11bb22", "cc33dd44", time.Minute,
		func(e model.DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if res.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0 — fixture should parse cleanly", res.ParseErrors)
	}
	if len(entries) != 333 {
		t.Fatalf("entries = %d, want 333 (333 change records; the trailing statistics envelope is skipped, not counted)", len(entries))
	}

	// Modifier histogram pins the fixture content. If regeneration diversifies
	// the fixture (add `-`/`U`/`T`/`MU` records), update these counts.
	counts := map[model.ModifierKind]int{}
	for _, e := range entries {
		counts[e.Kinds]++
	}
	if got := counts[model.KindModified]; got != 198 {
		t.Errorf("KindModified count = %d, want 198", got)
	}
	if got := counts[model.KindAdded]; got != 135 {
		t.Errorf("KindAdded count = %d, want 135", got)
	}

	// The first record locks the leading-/ requirement and the M classification.
	if first := entries[0]; first.Path != "/etc/app/app.conf" || first.Modifier != "M" || first.Kinds != model.KindModified {
		t.Errorf("first entry = %+v, want /etc/app/app.conf modifier=M Kinds=KindModified", first)
	}

	// The `statistics` envelope at the end must be silently skipped (parser
	// rule: unknown message_type, not a parse error). If a future restic
	// release changes its envelope or removes the trailing newline, this
	// assertion plus ParseErrors==0 above is what catches it.
	last := entries[len(entries)-1]
	if last.Path == "" || last.Modifier == "" {
		t.Errorf("last entry looks like the statistics envelope leaked: %+v", last)
	}
}

func TestStreamDiffParsesEntries(t *testing.T) {
	fs := &diffStreamFake{data: strings.Join([]string{
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`{"message_type":"change","path":"/b","modifier":"-"}`,
		`{"message_type":"statistics","added":{"files":1,"bytes":1},"removed":{"files":1,"bytes":1}}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}

	var entries []model.DiffEntry
	res, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		"old", "new", time.Minute,
		func(e model.DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if res.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0", res.ParseErrors)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	got := strings.Join(fs.gotArgs, " ")
	for _, want := range []string{"--no-lock", "diff", "--json", "old", "new"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

func TestStreamDiffOnEntryErrorAborts(t *testing.T) {
	fs := &diffStreamFake{data: strings.Join([]string{
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`{"message_type":"change","path":"/b","modifier":"+"}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}
	sentinel := errors.New("disk full")
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
		func(model.DiffEntry) error { return sentinel }, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel verbatim", err)
	}
}

func TestStreamDiffTolerateMalformed(t *testing.T) {
	fs := &diffStreamFake{data: strings.Join([]string{
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`not json`,
		`{"message_type":"change","path":"/b","modifier":"-"}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}
	var entries []model.DiffEntry
	res, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
		func(e model.DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if len(entries) != 2 || res.ParseErrors != 1 {
		t.Errorf("entries=%d parseErrors=%d, want 2/1", len(entries), res.ParseErrors)
	}
}

func TestStreamDiffResticFailureClassifies(t *testing.T) {
	// The diff path must classify exit codes the same way as Snapshots, so a
	// wrong-password or locked-repo failure surfaces a typed *resticx.Error
	// instead of leaking the raw exit-status string into the UI. Mirrors
	// TestClassifyExitCodes on the Snapshots path.
	tests := []struct {
		name   string
		exit   fakeExit
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExit(10), "repository does not exist", KindRepoNotFound},
		{"locked", fakeExit(11), string(readFixture(t, "restic-error-locked.stderr")), KindLocked},
		{"wrong password", fakeExit(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"unknown exit", fakeExit(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &diffStreamFake{err: tt.exit, stderr: []byte(tt.stderr)}
			c := &Client{Stream: fs}
			_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute, nil, nil)
			var re *Error
			if !asResticError(err, &re) {
				t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
			}
			if re.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", re.Kind, tt.want)
			}
		})
	}
}

// TestStreamDiffMissingBinaryClassifies asserts the diff path classifies a
// missing `restic` executable exactly like the Snapshots path does — a typed
// KindBinaryMissing error rather than the bare exec error. This is the same
// failure mode a user with PATH issues hits, so the surfaced message must be
// the friendly one.
func TestStreamDiffMissingBinaryClassifies(t *testing.T) {
	// ExecRunner with a guaranteed-empty PATH so `restic` cannot be found.
	t.Setenv("PATH", "")
	c := &Client{Runner: ExecRunner{}, Stream: ExecRunner{}}
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute, nil, nil)
	var re *Error
	if !asResticError(err, &re) {
		t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
	}
	if re.Kind != KindBinaryMissing {
		t.Errorf("Kind = %v, want KindBinaryMissing", re.Kind)
	}
}

func TestStreamDiffProgressFires(t *testing.T) {
	var lines []string
	for i := 0; i < 600; i++ {
		lines = append(lines, `{"message_type":"change","path":"/x","modifier":"+"}`)
	}
	fs := &diffStreamFake{data: strings.Join(lines, "\n") + "\n"}
	c := &Client{Stream: fs}
	var ticks []int
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
		func(model.DiffEntry) error { return nil }, func(seen int) { ticks = append(ticks, seen) })
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if len(ticks) < 2 {
		t.Errorf("ticks = %v, want at least 2", ticks)
	}
}

func TestStreamDiffUsesCallTimeout(t *testing.T) {
	fs := &diffStreamFake{block: true}
	c := &Client{Stream: fs, Timeout: time.Hour}
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Millisecond, nil, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout, got %v", err)
	}
}

func TestStreamDiffZeroCallTimeoutFallsBackToClientTimeout(t *testing.T) {
	fs := &diffStreamFake{block: true}
	c := &Client{Stream: fs, Timeout: time.Millisecond}
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", 0, nil, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout, got %v", err)
	}
}

func TestStreamDiffPasswordOutOfBand(t *testing.T) {
	fs := &diffStreamFake{data: ""}
	c := &Client{Stream: fs}
	if _, err := c.StreamDiff(context.Background(), testTarget,
		Creds{AccessKey: "AK", SecretKey: "SK", ResticPassword: "super-secret-pw"},
		"o", "n", time.Minute, nil, nil); err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	env := strings.Join(fs.gotEnv, "\n")
	if strings.Contains(env, "super-secret-pw") {
		t.Error("password leaked into env")
	}
	if !strings.Contains(env, "RESTIC_PASSWORD_FILE=/dev/fd/3") {
		t.Error("env should reference the password file on fd 3")
	}
}
