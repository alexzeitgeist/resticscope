package resticx

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"resticscope/internal/model"
)

// diffStreamFake feeds canned NDJSON to onStdout once, optionally returning a
// run error to exercise the classify path.
type diffStreamFake struct {
	data    string
	stderr  []byte
	err     error
	gotArgs []string
	gotEnv  []string
}

func (f *diffStreamFake) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) ([]byte, error) {
	f.gotEnv = env
	f.gotArgs = args
	cbErr := onStdout(strings.NewReader(f.data))
	if cbErr != nil {
		return f.stderr, cbErr
	}
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
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
		"old", "new",
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
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n",
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
	res, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n",
		func(e model.DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if len(entries) != 2 || res.ParseErrors != 1 {
		t.Errorf("entries=%d parseErrors=%d, want 2/1", len(entries), res.ParseErrors)
	}
}

func TestStreamDiffResticFailureClassifies(t *testing.T) {
	fs := &diffStreamFake{err: fakeExit(10), stderr: []byte("repository does not exist")}
	c := &Client{Stream: fs}
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n", nil, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindRepoNotFound {
		t.Fatalf("want KindRepoNotFound, got %v", err)
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
	_, err := c.StreamDiff(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "o", "n",
		func(model.DiffEntry) error { return nil }, func(seen int) { ticks = append(ticks, seen) })
	if err != nil {
		t.Fatalf("StreamDiff: %v", err)
	}
	if len(ticks) < 2 {
		t.Errorf("ticks = %v, want at least 2", ticks)
	}
}

func TestStreamDiffPasswordOutOfBand(t *testing.T) {
	fs := &diffStreamFake{data: ""}
	c := &Client{Stream: fs}
	if _, err := c.StreamDiff(context.Background(), testTarget,
		Creds{AccessKey: "AK", SecretKey: "SK", ResticPassword: "super-secret-pw"},
		"o", "n", nil, nil); err != nil {
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
