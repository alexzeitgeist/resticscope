package resticx

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// diffStreamFake supplies canned NDJSON and optional process failures.
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

// The golden fixture is synthetic, modeled on restic 0.18 `diff --json` output,
// and pins record fields, modifier semantics, and the terminal statistics
// envelope. It currently covers only `+` and `M`; extending it should add `-`,
// `U`, and `T` records.
func TestStreamDiffParsesFixture(t *testing.T) {
	fs := &diffStreamFake{data: string(readFixture(t, "restic-0.18-diff.ndjson"))}
	c := &Client{Stream: fs}

	var entries []model.DiffEntry
	res, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"},
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

	// Keep these counts synchronized with any fixture regeneration.
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

	if first := entries[0]; first.Path != "/etc/app/app.conf" || first.Modifier != "M" || first.Kinds != model.KindModified {
		t.Errorf("first entry = %+v, want /etc/app/app.conf modifier=M Kinds=KindModified", first)
	}

	// The terminal statistics envelope is not a change record or parse error.
	// ParseErrors above also detects envelope and trailing-newline drift.
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
	res, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"},
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
	_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
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
	res, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
		func(e model.DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if len(entries) != 2 || res.ParseErrors != 1 {
		t.Errorf("entries=%d parseErrors=%d, want 2/1", len(entries), res.ParseErrors)
	}
}

func TestStreamDiffResticFailureClassifies(t *testing.T) {
	// Match snapshot error classification so the UI receives typed failures
	// instead of raw exit-status strings.
	tests := []struct {
		name   string
		exit   fakeExitError
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExitError(10), "repository does not exist", KindRepoNotFound},
		{"locked", fakeExitError(11), string(readFixture(t, "restic-error-locked.stderr")), KindLocked},
		{"wrong password", fakeExitError(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"unknown exit", fakeExitError(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &diffStreamFake{err: tt.exit, stderr: []byte(tt.stderr)}
			c := &Client{Stream: fs}
			_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute, nil, nil)
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

// A missing restic executable must produce the same typed error on both diff
// and snapshot paths.
func TestStreamDiffMissingBinaryClassifies(t *testing.T) {
	t.Setenv("PATH", "")
	c := &Client{Runner: ExecRunner{}, Stream: ExecRunner{}}
	_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute, nil, nil)
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
	for range 600 {
		lines = append(lines, `{"message_type":"change","path":"/x","modifier":"+"}`)
	}
	fs := &diffStreamFake{data: strings.Join(lines, "\n") + "\n"}
	c := &Client{Stream: fs}
	var ticks []int
	_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Minute,
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
	_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", time.Millisecond, nil, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout, got %v", err)
	}
}

func TestStreamDiffZeroCallTimeoutFallsBackToClientTimeout(t *testing.T) {
	fs := &diffStreamFake{block: true}
	c := &Client{Stream: fs, Timeout: time.Millisecond}
	_, err := c.StreamDiff(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", 0, nil, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout, got %v", err)
	}
}

func TestStreamDiffPasswordOutOfBand(t *testing.T) {
	fs := &diffStreamFake{data: ""}
	c := &Client{Stream: fs}
	if _, err := c.StreamDiff(t.Context(), testTarget,
		Creds{Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}, ResticPassword: "super-secret-pw"},
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
