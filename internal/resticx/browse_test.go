package resticx

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"resticscope/internal/model"
)

// NDJSON fixtures: a leading snapshot record (struct_type != "node") followed by
// node records, exactly as `restic ls --json --recursive` emits them.
const (
	snapLine = `{"time":"2024-01-01T00:00:00Z","struct_type":"snapshot","id":"abcd"}`
	nodeHome = `{"name":"home","type":"dir","path":"/home","size":0,"struct_type":"node"}`
	nodeAlex = `{"name":"alex","type":"dir","path":"/home/alex","size":0,"struct_type":"node"}`
	nodeFile = `{"name":"f.txt","type":"file","path":"/home/alex/f.txt","size":42,"struct_type":"node"}`
)

func ndjson(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// fakeStream is an injected StreamRunner. It feeds onStdout canned NDJSON,
// optionally blocking after the data until the context is done (to exercise the
// timeout path), and records what it was asked to run plus the context state it
// observed once the callback returned.
type fakeStream struct {
	data   string
	stderr []byte
	err    error
	block  bool

	gotEnv      []string
	gotPassword string
	gotArgs     []string
	ctxErr      error
}

func (f *fakeStream) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) ([]byte, error) {
	f.gotEnv = env
	f.gotPassword = password
	f.gotArgs = args
	var r io.Reader = strings.NewReader(f.data)
	if f.block {
		r = &blockingReader{data: f.data, ctx: ctx}
	}
	cbErr := onStdout(r)
	f.ctxErr = ctx.Err()
	if cbErr != nil {
		return f.stderr, cbErr
	}
	return f.stderr, f.err
}

// blockingReader yields its data once, then blocks until the context is done and
// reports EOF — simulating restic stalling (in repo-open or mid-stream) until the
// browse deadline kills it and the pipe closes.
type blockingReader struct {
	data string
	off  int
	ctx  context.Context
	done bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.off < len(b.data) {
		n := copy(p, b.data[b.off:])
		b.off += n
		return n, nil
	}
	if b.done {
		return 0, io.EOF
	}
	<-b.ctx.Done()
	b.done = true
	return 0, io.EOF
}

func browseLimits() model.BrowseLimits {
	return model.BrowseLimits{
		MaxEntries:          1000,
		MaxJSONBytes:        1 << 20,
		Timeout:             5 * time.Second,
		MaxSessionEntries:   10000,
		MaxSessionJSONBytes: 8 << 20,
	}
}

func TestListSnapshotTreeComplete(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	scan, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseLimits())
	if err != nil {
		t.Fatalf("ListSnapshotTree: %v", err)
	}
	if scan.Reason != model.BrowseComplete {
		t.Errorf("reason = %v, want BrowseComplete", scan.Reason)
	}
	if scan.LoadedEntries != 3 || len(scan.Nodes) != 3 {
		t.Fatalf("loaded = %d / nodes = %d, want 3 (snapshot record must be skipped)", scan.LoadedEntries, len(scan.Nodes))
	}
	if scan.Frontier.Path != "/home/alex/f.txt" || scan.Frontier.IsDir {
		t.Errorf("frontier = %+v, want the last (file) node", scan.Frontier)
	}
	// The recursive, no-lock listing args must be present.
	got := strings.Join(fs.gotArgs, " ")
	for _, want := range []string{"--no-lock", "ls", "--json", "--recursive", "abcd", "/"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

func TestListSnapshotTreeEntryCap(t *testing.T) {
	lim := browseLimits()
	lim.MaxEntries = 2
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	scan, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", lim)
	if err != nil {
		t.Fatalf("entry cap should be a partial success, got error: %v", err)
	}
	if scan.Reason != model.PartialEntryCap {
		t.Errorf("reason = %v, want PartialEntryCap", scan.Reason)
	}
	if scan.LoadedEntries != 2 {
		t.Errorf("loaded = %d, want 2", scan.LoadedEntries)
	}
	if fs.ctxErr == nil {
		t.Error("hitting the entry cap should have cancelled the restic process")
	}
}

func TestListSnapshotTreeByteCap(t *testing.T) {
	lim := browseLimits()
	// Allow exactly the snapshot record + first node line (with newlines); the
	// next decode must hit the byte limit.
	lim.MaxJSONBytes = int64(len(snapLine) + 1 + len(nodeHome) + 1)
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	scan, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", lim)
	if err != nil {
		t.Fatalf("byte cap should be a partial success, got error: %v", err)
	}
	if scan.Reason != model.PartialByteCap {
		t.Errorf("reason = %v, want PartialByteCap", scan.Reason)
	}
	if scan.LoadedEntries < 1 {
		t.Errorf("loaded = %d, want >= 1", scan.LoadedEntries)
	}
	if fs.ctxErr == nil {
		t.Error("hitting the byte cap should have cancelled the restic process")
	}
}

func TestListSnapshotTreeTimeoutWithNodes(t *testing.T) {
	lim := browseLimits()
	lim.Timeout = 60 * time.Millisecond
	fs := &fakeStream{data: ndjson(snapLine, nodeHome), block: true}
	c := &Client{Stream: fs}
	scan, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", lim)
	if err != nil {
		t.Fatalf("timeout with nodes should be a partial success, got error: %v", err)
	}
	if scan.Reason != model.PartialTimeout {
		t.Errorf("reason = %v, want PartialTimeout", scan.Reason)
	}
	if scan.LoadedEntries != 1 {
		t.Errorf("loaded = %d, want 1", scan.LoadedEntries)
	}
}

func TestListSnapshotTreeTimeoutZeroNodesIsError(t *testing.T) {
	lim := browseLimits()
	lim.Timeout = 50 * time.Millisecond
	fs := &fakeStream{data: "", block: true} // stalls in repo-open, never a node
	c := &Client{Stream: fs}
	scan, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", lim)
	if err == nil {
		t.Fatal("timeout with zero nodes must return an error, not an empty scan")
	}
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout error, got %v", err)
	}
	if len(scan.Nodes) != 0 {
		t.Errorf("error path must return an empty scan, got %d nodes", len(scan.Nodes))
	}
}

func TestListSnapshotTreeResticFailureClassifies(t *testing.T) {
	fs := &fakeStream{err: fakeExit(10), stderr: []byte("repository does not exist")}
	c := &Client{Stream: fs}
	_, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseLimits())
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindRepoNotFound {
		t.Fatalf("want KindRepoNotFound, got %v", err)
	}
}

func TestListSnapshotTreeRedactsStderr(t *testing.T) {
	fs := &fakeStream{err: fakeExit(1), stderr: []byte("failed using key AK-LEAKED-9")}
	c := &Client{
		Stream: fs,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAKED-9", "[REDACTED]") },
	}
	_, err := c.ListSnapshotTree(context.Background(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseLimits())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "AK-LEAKED-9") {
		t.Errorf("stderr secret leaked into error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("expected redacted stderr, got %q", err.Error())
	}
}

func TestListSnapshotTreePasswordOutOfBand(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome)}
	c := &Client{Stream: fs}
	creds := Creds{AccessKey: "AK", SecretKey: "SK", ResticPassword: "super-secret-pw"}
	if _, err := c.ListSnapshotTree(context.Background(), testTarget, creds, "abcd", browseLimits()); err != nil {
		t.Fatalf("ListSnapshotTree: %v", err)
	}
	env := strings.Join(fs.gotEnv, "\n")
	if !strings.Contains(env, "RESTIC_PASSWORD_FILE=/dev/fd/3") {
		t.Error("env should reference the password file on fd 3")
	}
	if strings.Contains(env, "RESTIC_PASSWORD=") {
		t.Error("env must never set RESTIC_PASSWORD")
	}
	if strings.Contains(env, "super-secret-pw") {
		t.Error("password leaked into the environment")
	}
	if fs.gotPassword != "super-secret-pw" {
		t.Errorf("password not passed out-of-band, got %q", fs.gotPassword)
	}
}
