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

// NDJSON fixtures: a leading snapshot record (struct_type != "node") followed by
// node records, exactly as `restic ls --json --recursive` emits them.
const (
	snapLine = `{"time":"2024-01-01T00:00:00Z","struct_type":"snapshot","id":"abcd"}`
	nodeHome = `{"name":"home","type":"dir","path":"/home","size":0,"struct_type":"node"}`
	nodeAlex = `{"name":"alex","type":"dir","path":"/home/alex","size":0,"struct_type":"node"}`
	nodeFile = `{"name":"f.txt","type":"file","path":"/home/alex/f.txt","size":42,"struct_type":"node"}`
	nodeLink = `{"name":"link","type":"symlink","path":"/home/alex/link","linktarget":"/home/alex/f.txt","struct_type":"node"}`
	// nodeMeta carries the full per-node metadata restic 0.18.1 emits (mtime,
	// permissions, uid, gid) so the decode of the new fields can be asserted
	// without disturbing the byte-exact fixtures other tests depend on.
	nodeMeta = `{"name":"meta.txt","type":"file","path":"/home/alex/meta.txt","size":7,"uid":1000,"gid":1000,"mode":436,"permissions":"-rw-rw-r--","mtime":"2026-05-26T11:28:49Z","struct_type":"node"}`
)

func ndjson(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

const browseTimeout = 5 * time.Second

// collect returns an onNode callback that appends to nodes (returned by pointer).
func collect() (func(model.BrowseNode) error, *[]model.BrowseNode) {
	var nodes []model.BrowseNode
	return func(n model.BrowseNode) error {
		nodes = append(nodes, n)
		return nil
	}, &nodes
}

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
	// A real restic killed by the browse deadline returns a non-nil run error, not
	// a clean exit; mirror that so timeout classification sees a failed run.
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
}

// cancelingBadJSONStream simulates a stdout read/decode error racing with a user
// cancellation. The browse boundary must report the cancellation, not a parse
// error from the pipe closing under the decoder.
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

// cleanButExpiredStream feeds the whole tree, lets the browse deadline elapse, then
// reports a clean exit (nil run error) — modelling restic finishing and exiting 0
// just before the index deadline fired. The boundary must treat this as complete,
// not discard a fully-streamed tree as a partial.
type cleanButExpiredStream struct{ data string }

func (f cleanButExpiredStream) RunStream(ctx context.Context, _ []string, _ string, onStdout func(io.Reader) error, _ ...string) ([]byte, error) {
	if err := onStdout(strings.NewReader(f.data)); err != nil {
		return nil, err
	}
	<-ctx.Done() // the deadline fires after a clean, complete read
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
	if !sum.Complete {
		t.Errorf("Complete = false, want true")
	}
	if sum.Entries != 3 || len(*nodes) != 3 {
		t.Fatalf("entries = %d / nodes = %d, want 3 (snapshot record must be skipped)", sum.Entries, len(*nodes))
	}
	// The recursive, no-lock listing args must be present.
	got := strings.Join(fs.gotArgs, " ")
	for _, want := range []string{"--no-lock", "ls", "--json", "--recursive", "abcd", "/"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

// An empty snapshot is a clean run with zero nodes: it must report Complete=true
// so the caller indexes it (an empty index is valid), not an error.
func TestStreamSnapshotTreeEmptyIsComplete(t *testing.T) {
	fs := &fakeStream{data: ndjson(snapLine)}
	c := &Client{Stream: fs}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if err != nil {
		t.Fatalf("StreamSnapshotTree: %v", err)
	}
	if !sum.Complete || sum.Entries != 0 || len(*nodes) != 0 {
		t.Errorf("empty snapshot: Complete=%v entries=%d nodes=%d, want true/0/0", sum.Complete, sum.Entries, len(*nodes))
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

// A symlink's type and linktarget must decode into the node.
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

// A node that omits uid/gid must decode with OwnerKnown false (so the renderer
// shows a missing-owner em-dash, never a spurious 0:0).
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

// A callback error (e.g. the store's disk limit) is surfaced verbatim so the
// caller can classify it, and it cancels the restic process.
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
	if sum.Complete {
		t.Error("Complete must be false when the callback failed")
	}
	if fs.ctxErr == nil {
		t.Error("a callback error should have cancelled the restic process")
	}
}

// A user leaving the browser cancels the parent context; the in-flight store write
// (tx.Add on the cancelled ctx) then fails with the store's own interrupt error,
// NOT context.Canceled. The boundary must still classify this as a cancel so the
// caller can tell it apart from a genuine store failure — which leaves the parent
// ctx live and is surfaced verbatim (see TestStreamSnapshotTreeCallbackErrorCancels).
func TestStreamSnapshotTreeUserCancelBeatsCallbackError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	storeInterrupt := errors.New("interrupted (SQLITE_INTERRUPT)")
	onNode := func(model.BrowseNode) error {
		cancel() // the user left mid-index
		return storeInterrupt
	}
	sum, err := c.StreamSnapshotTree(ctx, testTarget, Creds{ResticPassword: "pw"}, "abcd", browseTimeout, onNode)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("user cancel must classify as context.Canceled, got %v", err)
	}
	if errors.Is(err, storeInterrupt) {
		t.Fatalf("the store's interrupt error masked the user cancel: %v", err)
	}
	if sum.Complete {
		t.Error("Complete must be false on cancel")
	}
}

// A clean, complete restic exit must win even if the index deadline elapsed in the
// gap between restic exiting 0 and the classification running; otherwise a fully
// streamed tree is wrongly rolled back as a partial and re-indexed on the next browse.
func TestStreamSnapshotTreeCleanExitBeatsExpiredDeadline(t *testing.T) {
	c := &Client{Stream: cleanButExpiredStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}}
	onNode, nodes := collect()
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", 50*time.Millisecond, onNode)
	if err != nil {
		t.Fatalf("a clean, complete exit must not error even if the deadline elapsed after it: %v", err)
	}
	if !sum.Complete {
		t.Fatal("Complete = false: a fully-streamed snapshot was discarded because the deadline fired after the clean exit")
	}
	if sum.Entries != 3 || len(*nodes) != 3 {
		t.Fatalf("entries = %d / nodes = %d, want 3", sum.Entries, len(*nodes))
	}
}

// A genuine store error (e.g. the disk limit) that races the browse deadline after
// ≥1 accepted node must surface verbatim, NOT be downgraded to an incomplete/partial
// stream. The callback writes through the PARENT ctx (tx.Add(ctx, …)), which the
// browse timeout never cancels, so a callback error with the parent ctx still live is
// always a real failure — a non-retryable disk limit must not be masked as a
// retryable timeout. Guards against st.cbErr being ordered after the deadline cases.
func TestStreamSnapshotTreeCallbackErrorBeatsDeadline(t *testing.T) {
	timeout := 40 * time.Millisecond
	fs := &fakeStream{data: ndjson(snapLine, nodeHome, nodeAlex, nodeFile)}
	c := &Client{Stream: fs}
	diskFull := errors.New("browse store disk limit exceeded")
	calls := 0
	onNode := func(model.BrowseNode) error {
		calls++
		if calls == 2 {
			time.Sleep(timeout + 20*time.Millisecond) // let the browse deadline elapse first
			return diskFull
		}
		return nil
	}
	sum, err := c.StreamSnapshotTree(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "abcd", timeout, onNode)
	if !errors.Is(err, diskFull) {
		t.Fatalf("a store error racing the deadline must surface verbatim, got %v", err)
	}
	if sum.Complete {
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
	if sum.Complete {
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
	// A genuine JSON decode failure (not a clean EOF) must surface as KindParse,
	// not a generically-classified run error.
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
