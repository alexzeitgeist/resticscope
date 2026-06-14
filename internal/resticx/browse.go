package resticx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"

	"resticscope/internal/model"

	json "github.com/goccy/go-json"
)

// browseStreamBuffer matches the benchmark harness' reader size. It keeps the
// JSON decoder from reading tiny chunks from restic's stdout pipe on huge trees.
const browseStreamBuffer = 1 << 20

// browse.go is the streaming restic boundary for the in-app snapshot browser. It
// runs a single `restic --no-lock ls --json --recursive <snap> /` and hands each
// decoded node to a caller-supplied callback as it arrives. The expensive part of
// any restic invocation is opening the repo (index/lock load); returning more
// data is cheap. So browse pays that cost once and streams the whole namespace,
// rather than listing lazily per directory (which would re-pay repo-open on every
// keystroke). The callback persists each node into the encrypted browse store; no
// path data is retained here or returned in the summary.

// lsNode mirrors the subset of `restic ls --json` records browse needs. restic
// emits a leading snapshot record (struct_type != "node") then one record per
// filesystem node; we keep only nodes. The JSON contract is additive, so unknown
// fields are ignored. mtime decodes straight into a time.Time (restic emits
// RFC3339, which Go's JSON unmarshals). uid/gid are pointers so a node that
// omits them is distinguishable from a real root-owned node (uid=0,gid=0).
// linktarget is present only for symlinks.
type lsNode struct {
	StructType  string    `json:"struct_type"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mtime"`
	Permissions string    `json:"permissions"`
	UID         *uint32   `json:"uid"`
	GID         *uint32   `json:"gid"`
	LinkTarget  string    `json:"linktarget"`
}

// StreamSnapshotTree streams a recursive listing of one snapshot's full namespace,
// invoking onNode for each decoded node. It owns its own timeout context
// (timeout), independent of the Client's refresh budget, because a deep browse may
// legitimately run longer than a refresh. It retains nothing: the returned summary
// reports only the node count and whether restic emitted the whole tree.
//
// Classification is deliberate and ordered. It keys cancel off the parent ctx and
// completion off the run result. The browse deadline lives on bctx, which restic
// runs under but the onNode callback does NOT — the callback writes through the
// parent ctx (app/browsesession.go: tx.Add(ctx, …)). So a deadline never interrupts
// a store write; only a parent-ctx cancel does. That asymmetry drives the order:
//  1. the caller cancelled the browse context → context.Canceled, even if onNode
//     failed (a parent cancel reaches tx.Add and surfaces as the store's own
//     interrupt error — a downstream symptom, not a real store failure; the parent
//     ctx, not bctx, is what tells this apart from a genuine onNode failure in (2));
//  2. onNode returned an error → return it verbatim, so the caller can tell its own
//     store/disk-limit failure (e.g. errors.Is(model.ErrBrowseDiskLimit)) from a
//     restic failure. This precedes the deadline checks because the callback never
//     sees bctx: with the parent ctx still live, a callback error is always a real
//     failure, so a store error racing the deadline must not be downgraded to a
//     retryable partial;
//  3. a clean run (no run or decode error) → {IsComplete:true}, nil, including an
//     empty snapshot (count 0); precedes the deadline checks so a stream that exited
//     0 just before the index deadline elapsed is reported complete, not rolled back
//     as a partial;
//  4. the deadline fired after ≥1 node → {IsComplete:false}, nil (a partial stream
//     the caller must not mark indexed; reaching here means restic was killed by the
//     deadline with no callback error — a genuine store error is handled in (2));
//  5. any other deadline result → KindTimeout;
//  6. a genuine JSON decode failure → KindParse;
//  7. anything else (restic failure) → classify.
func (c *Client) StreamSnapshotTree(ctx context.Context, t Target, creds Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := prependBackendOpts(t, "--no-lock", "ls", "--json", "--recursive", snapshotID, "/")

	runner, err := c.streamRunner()
	if err != nil {
		return model.BrowseScanSummary{}, err
	}

	env := c.buildEnv(t, creds)
	st := &browseStream{onNode: onNode, cancel: cancel}
	stderr, runErr := runner.RunStream(bctx, env, creds.ResticPassword, st.consume, full...)

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// The caller cancelled the browse (e.g. left the browser). Report a clean
		// cancel even when st.cbErr is set: the cancel propagated into the in-flight
		// onNode → tx.Add on the parent ctx, where the store returns its own interrupt
		// error rather than context.Canceled. Keying off the parent ctx is what
		// separates a user cancel from a genuine onNode failure below, which leaves
		// the parent ctx live.
		return model.BrowseScanSummary{Entries: st.count}, context.Canceled
	case st.cbErr != nil:
		// onNode itself failed (disk limit / store write error); surface it verbatim
		// so the caller can tell errors.Is(model.ErrBrowseDiskLimit) from a restic
		// failure. This precedes the deadline checks deliberately, because the callback writes
		// through the parent ctx (tx.Add(ctx, …)), which the browse timeout never
		// cancels — so a cbErr with the parent ctx still live is always a real failure,
		// never a timeout symptom, and a store error racing the deadline must not be
		// downgraded to a retryable partial.
		return model.BrowseScanSummary{Entries: st.count}, st.cbErr
	case runErr == nil && st.decodeErr == nil:
		// A clean run: restic emitted the whole tree and exited 0 (an empty snapshot,
		// count 0, included). This precedes the deadline checks to ensure a stream that
		// finished cleanly just before the index deadline elapsed is reported complete
		// instead of being discarded as a partial. (cbErr is already handled above; the
		// decodeErr guard keeps a parse failure that raced a clean exit from slipping
		// through as complete.)
		return model.BrowseScanSummary{Entries: st.count, IsComplete: true}, nil
	case errors.Is(bctx.Err(), context.DeadlineExceeded) && st.count > 0:
		// The index deadline fired mid-stream after ≥1 node and restic was killed with
		// no callback error (a genuine store error is returned verbatim above) — a
		// partial tree that must be left unindexed.
		return model.BrowseScanSummary{Entries: st.count, IsComplete: false}, nil
	case errors.Is(bctx.Err(), context.DeadlineExceeded):
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	case st.decodeErr != nil:
		// A genuine JSON decode failure is the real cause; surface it as KindParse
		// rather than letting classify mislabel the generic run state.
		return model.BrowseScanSummary{Entries: st.count}, &Error{Kind: KindParse, Op: "ls", wrapped: st.decodeErr}
	default:
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	}
}

// ErrNoStreamRunner is returned by the streaming methods when a Client has no
// stream-capable runner wired — neither Stream nor a Runner that implements
// StreamRunner. Production wires Stream explicitly, so it signals a miswire.
var ErrNoStreamRunner = errors.New("resticx: no stream runner configured (set Client.Stream)")

// streamRunner picks the StreamRunner for a streaming call: the Stream seam if
// set, else a Runner that also implements StreamRunner. It returns
// ErrNoStreamRunner rather than defaulting to a real ExecRunner, so a
// buffered-only Client fails the call instead of silently spawning restic.
func (c *Client) streamRunner() (StreamRunner, error) {
	if c.Stream != nil {
		return c.Stream, nil
	}
	if c.Runner != nil {
		if sr, ok := c.Runner.(StreamRunner); ok {
			return sr, nil
		}
	}
	return nil, ErrNoStreamRunner
}

// browseStream decodes the NDJSON stream and forwards each node to onNode. It
// keeps no node data: only a processed count and the first callback/decode error.
type browseStream struct {
	onNode func(model.BrowseNode) error
	cancel context.CancelFunc

	count     int   // nodes successfully handed to onNode
	cbErr     error // onNode's error (caller/store/cancel), surfaced verbatim
	decodeErr error // a genuine JSON decode failure (not a clean EOF)
}

// consume reads the NDJSON stream and calls onNode for each filesystem node. An
// onNode error records cbErr, cancels the process so restic stops producing, and
// returns. A clean EOF returns nil; a genuine decode error is recorded and
// returned for the caller to classify.
func (s *browseStream) consume(r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, browseStreamBuffer))
	for {
		var n lsNode
		err := dec.Decode(&n)
		if err == nil {
			if n.StructType != "node" {
				continue // leading snapshot record
			}
			node := model.BrowseNode{
				Path: n.Path, Name: n.Name, Type: n.Type, LinkTarget: n.LinkTarget,
				IsDir: n.Type == model.NodeTypeDir, Size: n.Size, ModTime: n.ModTime, Permissions: n.Permissions,
			}
			if n.UID != nil && n.GID != nil {
				node.UID = *n.UID
				node.GID = *n.GID
				node.OwnerKnown = true
			}
			if cbErr := s.onNode(node); cbErr != nil {
				s.cbErr = cbErr
				s.cancel()
				return cbErr
			}
			s.count++
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		s.decodeErr = err
		return err
	}
}
