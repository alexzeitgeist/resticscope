package resticx

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"resticscope/internal/model"
)

// browse.go is the streaming restic boundary for the in-app snapshot browser. It
// runs a single `restic --no-lock ls --json --recursive <snap> /` and streams the
// NDJSON into a capped, ordered node slice. The expensive part of any restic
// invocation is opening the repo (index/lock load); returning more data is cheap.
// So browse pays that cost once and reads a lot, bounded by entry/byte/time caps,
// rather than listing lazily per directory (which would re-pay repo-open on every
// keystroke). The result is a model.BrowseScan; building it into a tree is the
// pure model layer's job. No path data is persisted anywhere.

// errBrowsePartial is the sentinel the stream callback returns when it stops the
// crawl at a configured entry or byte cap. It is a deliberate stop, not a
// failure, so ListSnapshotTree turns it into a partial scan rather than an error.
var errBrowsePartial = errors.New("resticx: browse cap reached")

// errBrowseByteLimit is returned by byteLimitReader once the JSON byte cap is
// reached, so the JSON decoder unwinds and the callback can classify the stop.
var errBrowseByteLimit = errors.New("resticx: browse byte limit reached")

// lsNode mirrors the subset of `restic ls --json` records browse needs. restic
// emits a leading snapshot record (struct_type != "node") then one record per
// filesystem node; we keep only nodes. The JSON contract is additive, so unknown
// fields are ignored.
type lsNode struct {
	StructType string `json:"struct_type"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
}

// ListSnapshotTree streams a recursive listing of one snapshot's full namespace
// into a model.BrowseScan, bounded by limits. It owns its own timeout context
// (limits.Timeout), independent of the Client's refresh budget, because a deep
// browse may legitimately run longer than a refresh.
//
// Classification is deliberate and ordered (a cap-cancel surfaces as
// context.Canceled, which classify would otherwise call KindUnknown):
//  1. we hit an entry/byte cap and decoded ≥1 node → return the partial scan, nil;
//  2. the deadline fired and we decoded ≥1 node → return a PartialTimeout scan, nil;
//  3. a clean run decoded ≥1 node → return a BrowseComplete scan, nil;
//  4. anything else (timeout during repo-open, restic failure, zero usable nodes)
//     → return the classified, redacted *Error, never an empty scan, so the TUI
//     never opens an empty, misleading tree.
func (c *Client) ListSnapshotTree(ctx context.Context, t Target, creds Creds, snapshotID string, limits model.BrowseLimits) (model.BrowseScan, error) {
	bctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	full := make([]string, 0, 8)
	if t.BucketLookup == "dns" || t.BucketLookup == "path" {
		full = append(full, "-o", "s3.bucket-lookup="+t.BucketLookup)
	}
	full = append(full, "--no-lock", "ls", "--json", "--recursive", snapshotID, "/")

	env := c.buildEnv(t, creds)
	st := &browseStream{limits: limits, cancel: cancel}
	stderr, runErr := c.Stream.RunStream(bctx, env, creds.ResticPassword, st.consume, full...)

	switch {
	case st.capped && len(st.nodes) > 0:
		return st.scan(), nil
	case bctx.Err() == context.DeadlineExceeded && len(st.nodes) > 0:
		st.reason = model.PartialTimeout
		return st.scan(), nil
	case runErr == nil && st.decodeErr == nil && len(st.nodes) > 0:
		st.reason = model.BrowseComplete
		return st.scan(), nil
	default:
		return model.BrowseScan{}, c.classify(bctx, "ls", runErr, stderr)
	}
}

// browseStream accumulates the streamed nodes and records why the crawl stopped.
type browseStream struct {
	limits model.BrowseLimits
	cancel context.CancelFunc

	nodes     []model.BrowseNode
	bytesRead int64
	reason    model.PartialReason
	frontier  model.BrowseFrontier
	capped    bool  // stopped at an entry/byte cap (a deliberate stop)
	decodeErr error // a genuine JSON decode failure (not a cap or EOF)
}

// consume reads the NDJSON stream, appending each node and tracking the frontier
// (the last node seen). It stops at the entry cap or the byte cap by recording
// the reason, cancelling the process so restic stops producing, and returning
// errBrowsePartial. A clean EOF returns nil; a genuine decode error is recorded
// and returned for the caller to classify.
func (s *browseStream) consume(r io.Reader) error {
	reader := &byteLimitReader{r: r, limit: s.limits.MaxJSONBytes}
	dec := json.NewDecoder(reader)
	for {
		var n lsNode
		err := dec.Decode(&n)
		if err == nil {
			if n.StructType != "node" {
				continue // leading snapshot record
			}
			node := model.BrowseNode{Path: n.Path, Name: n.Name, IsDir: n.Type == "dir", Size: n.Size}
			s.nodes = append(s.nodes, node)
			s.frontier = model.BrowseFrontier{Path: node.Path, IsDir: node.IsDir}
			if s.limits.MaxEntries > 0 && len(s.nodes) >= s.limits.MaxEntries {
				s.reason = model.PartialEntryCap
				s.capped = true
				s.bytesRead = reader.read
				s.cancel()
				return errBrowsePartial
			}
			continue
		}
		s.bytesRead = reader.read
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errBrowseByteLimit) || reader.hitLimit {
			s.reason = model.PartialByteCap
			s.capped = true
			s.cancel()
			return errBrowsePartial
		}
		s.decodeErr = err
		return err
	}
}

// scan snapshots the accumulated state as a model.BrowseScan.
func (s *browseStream) scan() model.BrowseScan {
	return model.BrowseScan{
		Nodes:         s.nodes,
		Reason:        s.reason,
		LoadedEntries: len(s.nodes),
		JSONBytes:     s.bytesRead,
		Frontier:      s.frontier,
	}
}

// byteLimitReader caps the bytes read from the wrapped reader. Once limit bytes
// have been read it returns errBrowseByteLimit and sets hitLimit, so the JSON
// decoder unwinds rather than reading an unbounded stream. A limit <= 0 disables
// the cap. It is a reimplementation of the equivalent in tools/restic-ls-poc;
// product code must not import the dev tool.
type byteLimitReader struct {
	r        io.Reader
	limit    int64
	read     int64
	hitLimit bool
}

func (r *byteLimitReader) Read(p []byte) (int, error) {
	if r.limit > 0 {
		remaining := r.limit - r.read
		if remaining <= 0 {
			r.hitLimit = true
			return 0, errBrowseByteLimit
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := r.r.Read(p)
	r.read += int64(n)
	return n, err
}
