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
// Classification is deliberate and ordered:
//  1. onNode returned an error → return it verbatim, so the caller can tell its own
//     store/disk-limit/cancel failure (e.g. errors.Is(model.ErrBrowseDiskLimit))
//     from a restic failure;
//  2. the caller cancelled the browse context → context.Canceled;
//  3. the deadline fired after ≥1 node → {Complete:false}, nil (a partial stream;
//     the caller must not mark it indexed);
//  4. any other deadline result → KindTimeout;
//  5. a clean run (no run/decode error) → {Complete:true}, nil, including an empty
//     snapshot (count 0) — a clean exit unambiguously means restic finished;
//  6. a genuine JSON decode failure → KindParse;
//  7. anything else (restic failure) → classify.
func (c *Client) StreamSnapshotTree(ctx context.Context, t Target, creds Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := make([]string, 0, 8)
	if t.BucketLookup == "dns" || t.BucketLookup == "path" {
		full = append(full, "-o", "s3.bucket-lookup="+t.BucketLookup)
	}
	full = append(full, "--no-lock", "ls", "--json", "--recursive", snapshotID, "/")

	env := c.buildEnv(t, creds)
	st := &browseStream{onNode: onNode, cancel: cancel}
	stderr, runErr := c.streamRunner().RunStream(bctx, env, creds.ResticPassword, st.consume, full...)

	switch {
	case st.cbErr != nil:
		return model.BrowseScanSummary{Entries: st.count}, st.cbErr
	case errors.Is(bctx.Err(), context.Canceled):
		return model.BrowseScanSummary{Entries: st.count}, context.Canceled
	case errors.Is(bctx.Err(), context.DeadlineExceeded) && st.count > 0:
		return model.BrowseScanSummary{Entries: st.count, Complete: false}, nil
	case errors.Is(bctx.Err(), context.DeadlineExceeded):
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	case runErr == nil && st.decodeErr == nil:
		return model.BrowseScanSummary{Entries: st.count, Complete: true}, nil
	case st.decodeErr != nil:
		// A genuine JSON decode failure is the real cause; surface it as KindParse
		// rather than letting classify mislabel the generic run state.
		return model.BrowseScanSummary{Entries: st.count}, &Error{Kind: KindParse, Op: "ls", wrapped: st.decodeErr}
	default:
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	}
}

// streamRunner returns the StreamRunner to use for a browse crawl. The streaming
// seam (Stream) is preferred; when it is unset, a Runner that also implements
// StreamRunner is used, and finally the production ExecRunner. This lets a Client
// wired with only Runner still browse without a separate Stream assignment.
func (c *Client) streamRunner() StreamRunner {
	if c.Stream != nil {
		return c.Stream
	}
	if c.Runner != nil {
		if sr, ok := c.Runner.(StreamRunner); ok {
			return sr
		}
	}
	return ExecRunner{}
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
				IsDir: n.Type == "dir", Size: n.Size, ModTime: n.ModTime, Permissions: n.Permissions,
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
