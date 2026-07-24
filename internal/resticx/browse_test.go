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

// NDJSON fixtures: a leading snapshot record (struct_type != "node") followed by
// node records, exactly as `restic ls --json --recursive` emits them.
const (
	snapLine = `{"time":"2024-01-01T00:00:00Z","struct_type":"snapshot","id":"abcd"}`
	nodeHome = `{"name":"home","type":"dir","path":"/home","size":0,"struct_type":"node"}`
	nodeAlex = `{"name":"alex","type":"dir","path":"/home/alex","size":0,"struct_type":"node"}`
	nodeFile = `{"name":"f.txt","type":"file","path":"/home/alex/f.txt","size":42,"struct_type":"node"}`
	nodeLink = `{"name":"link","type":"symlink","path":"/home/alex/link","linktarget":"/home/alex/f.txt","struct_type":"node"}`
	// nodeMeta adds restic 0.18.1 ownership and file metadata for the dedicated
	// decode assertions below.
	nodeMeta = `{"name":"meta.txt","type":"file","path":"/home/alex/meta.txt","size":7,"uid":1000,"gid":1000,"mode":436,"permissions":"-rw-rw-r--","mtime":"2026-05-26T11:28:49Z","struct_type":"node"}`
)

func ndjson(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

const browseTimeout = 5 * time.Second

func collect() (func(model.BrowseNode) error, *[]model.BrowseNode) {
	var nodes []model.BrowseNode
	return func(n model.BrowseNode) error {
		nodes = append(nodes, n)
		return nil
	}, &nodes
}

// fakeStream supplies canned NDJSON, can block until cancellation, and records
// the invocation and post-callback context state.
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
	// Mirror the failed exit of a real restic process killed at the deadline.
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
}

// cancelingBadJSONStream races user cancellation with a truncated JSON record.
type cancelingBadJSONStream struct {
	cancel context.CancelFunc
}

func (f cancelingBadJSONStream) RunStream(ctx context.Context, _ []string, _ string, onStdout func(io.Reader) error, _ ...string) ([]byte, error) {
	err := onStdout(&cancelingBadJSONReader{cancel: f.cancel})
	if err != nil {
		return nil, err
	}
	return nil, ctx.Err()
}

type cancelingBadJSONReader struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancelingBadJSONReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	r.cancel()
	return copy(p, "{"), nil
}

// blockingReader yields its data, then simulates a stalled restic process until
// cancellation closes its pipe.
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

// cleanButExpiredStream models restic finishing just before the index deadline,
// with classification occurring after the deadline.
type cleanButExpiredStream struct{ data string }

func (f cleanButExpiredStream) RunStream(ctx context.Context, _ []string, _ string, onStdout func(io.Reader) error, _ ...string) ([]byte, error) {
	if err := onStdout(strings.NewReader(f.data)); err != nil {
		return nil, err
	}
	<-ctx.Done() // Delay the clean exit until after the deadline.
	return nil, nil
}

func TestStreamSnapshotTreeComplete(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	if !sum.IsComplete {
		t.Errorf("Complete = false, want true")
	}
	if sum.Entries != 3 || len(*nodes) != 3 {
		t.Fatalf("entries = %d / nodes = %d, want 3 (snapshot record must be skipped)", sum.Entries, len(*nodes))
	}
	got := strings.Join(fs.gotArgs, " ")
	for _, want := range []string{"--no-lock", "ls", "--json", "--recursive", "abcd", "/"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

// An empty snapshot is complete so callers can retain its valid empty index.
func TestStreamSnapshotTreeEmptyIsComplete(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	if !sum.IsComplete || sum.Entries != 0 || len(*nodes) != 0 {
		t.Errorf("empty snapshot: Complete=%v entries=%d nodes=%d, want true/0/0", sum.IsComplete, sum.Entries, len(*nodes))
	}
}

func TestStreamSnapshotTreeParsesMetadata(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeMeta)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	if _, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode); err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	var got *model.BrowseNode
	for i := range *nodes {
		if (*nodes)[i].Path == "/home/alex/meta.txt" {
			got = &(*nodes)[i]
		}
	}
	if got == nil {
		t.Fatalf("meta.txt node missing from %d nodes", len(*nodes))
	}
	wantMod := time.Date(2026, 5, 26, 11, 28, 49, 0, time.UTC)
	if !got.ModTime.Equal(wantMod) {
		t.Errorf("ModTime = %v, want %v", got.ModTime, wantMod)
	}
	if got.Permissions != "-rw-rw-r--" {
		t.Errorf("Permissions = %q, want -rw-rw-r--", got.Permissions)
	}
	if got.Type != "file" || got.IsDir {
		t.Errorf("Type = %q IsDir = %v, want file/false", got.Type, got.IsDir)
	}
	if !got.OwnerKnown || got.UID != 1000 || got.GID != 1000 {
		t.Errorf("owner = %d:%d (known=%v), want 1000:1000 (known)", got.UID, got.GID, got.OwnerKnown)
	}
}

func TestStreamSnapshotTreeParsesSymlink(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeLink)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	if _, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode); err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	if len(*nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(*nodes))
	}
	n := (*nodes)[0]
	if n.Type != "symlink" || n.LinkTarget != "/home/alex/f.txt" || n.IsDir {
		t.Errorf("symlink decode = Type %q LinkTarget %q IsDir %v", n.Type, n.LinkTarget, n.IsDir)
	}
}

// Missing ownership must remain unknown rather than appearing as root ownership.
func TestStreamSnapshotTreeMissingOwnerNotKnown(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	if _, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode); err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	if len(*nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(*nodes))
	}
	if (*nodes)[0].OwnerKnown {
		t.Errorf("a node without uid/gid must not be OwnerKnown: %+v", (*nodes)[0])
	}
}

// Callback errors remain classifiable and stop the restic process.
func TestStreamSnapshotTreeCallbackErrorCancels(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	sentinel := errors.New("store is full")
	calls := 0
	onNode := func(model.BrowseNode) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	}
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel verbatim", err)
	}
	if sum.IsComplete {
		t.Error("Complete must be false when the callback failed")
	}
	if fs.ctxErr == nil {
		t.Error("a callback error should have cancelled the restic process")
	}
}

// A store interrupt caused by parent cancellation remains a user cancellation;
// genuine store failures occur while the parent context is live.
func TestStreamSnapshotTreeUserCancelBeatsCallbackError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	storeInterrupt := errors.New("interrupted (SQLITE_INTERRUPT)")
	onNode := func(model.BrowseNode) error {
		cancel() // Simulate the user leaving during indexing.
		return storeInterrupt
	}
	sum, err := c.StreamSnapshotTree(ctx, testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("user cancel must classify as context.Canceled, got %v", err)
	}
	if errors.Is(err, storeInterrupt) {
		t.Fatalf("the store's interrupt error masked the user cancel: %v", err)
	}
	if sum.IsComplete {
		t.Error("Complete must be false on cancel")
	}
}

// A clean restic exit wins a race with the deadline so a fully streamed tree is
// not discarded and indexed again.
func TestStreamSnapshotTreeCleanExitBeatsExpiredDeadline(t *testing.T) {
	c := &Client{Stream: cleanButExpiredStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", 50*time.Millisecond, onNode)
	if err != nil {
		t.Fatalf("a clean, complete exit must not error even if the deadline elapsed after it: %v", err)
	}
	if !sum.IsComplete {
		t.Fatal("Complete = false: a fully-streamed snapshot was discarded because the deadline fired after the clean exit")
	}
	if sum.Entries != 3 || len(*nodes) != 3 {
		t.Fatalf("entries = %d / nodes = %d, want 3", sum.Entries, len(*nodes))
	}
}

// A callback uses the parent context, so an error racing the browse deadline is a
// genuine store failure and must not be downgraded to a retryable partial stream.
func TestStreamSnapshotTreeCallbackErrorBeatsDeadline(t *testing.T) {
	timeout := 40 * time.Millisecond
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	diskFull := errors.New("browse store disk limit exceeded")
	calls := 0
	onNode := func(model.BrowseNode) error {
		calls++
		if calls == 2 {
			time.Sleep(timeout + 20*time.Millisecond) // Let the browse deadline elapse first.
			return diskFull
		}
		return nil
	}
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", timeout, onNode)
	if !errors.Is(err, diskFull) {
		t.Fatalf("a store error racing the deadline must surface verbatim, got %v", err)
	}
	if sum.IsComplete {
		t.Error("Complete must be false when the store failed")
	}
}

func TestStreamSnapshotTreeTimeoutWithNodes(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome), block: true}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", 60*time.Millisecond, onNode)
	if err != nil {
		t.Fatalf("timeout with nodes should be a partial success, got error: %v", err)
	}
	if sum.IsComplete {
		t.Errorf("Complete = true, want false (timeout truncated the stream)")
	}
	if sum.Entries != 1 || len(*nodes) != 1 {
		t.Errorf("entries = %d / nodes = %d, want 1", sum.Entries, len(*nodes))
	}
}

func TestStreamSnapshotTreeTimeoutZeroNodesIsError(t *testing.T) {
	fs := &fakeStream{data: "", block: true} // stalls in repo-open, never a node
	c := &Client{Stream: fs}
	onNode, _ := collect()
	_, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", 50*time.Millisecond, onNode)
	if err == nil {
		t.Fatal("timeout with zero nodes must return an error")
	}
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("want KindTimeout error, got %v", err)
	}
}

func TestStreamSnapshotTreeCancelBeatsDecodeError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &Client{Stream: cancelingBadJSONStream{cancel: cancel}}
	onNode, _ := collect()

	_, err := c.StreamSnapshotTree(ctx, testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	var re *Error
	if asResticError(err, &re) && re.Kind == KindParse {
		t.Fatalf("cancel raced with decode and was misclassified as parse: %v", err)
	}
}

func TestStreamSnapshotTreeMalformedJSONIsParseError(t *testing.T) {
	fs := &fakeStream{data: snapLine + "\nnot json at all\n"}
	c := &Client{Stream: fs}
	onNode, _ := collect()
	_, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if err == nil {
		t.Fatal("malformed JSON must return an error")
	}
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindParse {
		t.Fatalf("want KindParse error, got %v", err)
	}
}

func TestStreamSnapshotTreeResticFailureClassifies(t *testing.T) {
	fs := &fakeStream{err: fakeExitError(10), stderr: []byte("repository does not exist")}
	c := &Client{Stream: fs}
	onNode, _ := collect()
	_, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindRepoNotFound {
		t.Fatalf("want KindRepoNotFound, got %v", err)
	}
}

func TestStreamSnapshotTreeRedactsStderr(t *testing.T) {
	fs := &fakeStream{err: fakeExitError(1), stderr: []byte("failed using key AK-LEAKED-9")}
	c := &Client{
		Stream: fs,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAKED-9", "[REDACTED]") },
	}
	onNode, _ := collect()
	_, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
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

func TestStreamSnapshotTreePasswordOutOfBand(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine, nodeHome)}
	c := &Client{Stream: fs}
	creds := Creds{Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}, ResticPassword: "super-secret-pw"}
	onNode, _ := collect()
	if _, err := c.StreamSnapshotTree(t.Context(), testTarget, creds, "abcd", browseTimeout, onNode); err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
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
