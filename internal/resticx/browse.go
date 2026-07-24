package resticx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"

	json "github.com/goccy/go-json"
)

// browseStreamBuffer reduces decoder reads from restic when scanning large trees.
const browseStreamBuffer = 1 << 20

// Opening a repository dominates restic's cost, so browsing streams the entire
// namespace once. Decoded nodes pass to the caller and are not retained here.

// lsNode mirrors the fields used from `restic ls --json`. Pointer ownership IDs
// distinguish omitted values from root ownership; LinkTarget applies to symlinks.
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

// StreamSnapshotTree passes each node in a snapshot's namespace to onNode. Its
// timeout is independent of the client's refresh budget. The returned summary
// contains only the node count and whether restic emitted the complete tree.
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
		// The store may report its own interrupt from onNode after caller cancellation.
		// The parent context distinguishes that case from a genuine callback failure.
		return model.BrowseScanSummary{Entries: st.count}, context.Canceled
	case st.cbErr != nil:
		// Preserve callback errors, including typed store failures. onNode uses the
		// parent context, so its errors cannot be browse-timeout symptoms.
		return model.BrowseScanSummary{Entries: st.count}, st.cbErr
	case runErr == nil && st.decodeErr == nil:
		// Check success before the deadline so a just-completed stream remains complete.
		return model.BrowseScanSummary{Entries: st.count, IsComplete: true}, nil
	case errors.Is(bctx.Err(), context.DeadlineExceeded) && st.count > 0:
		// A timed-out partial tree must remain unindexed.
		return model.BrowseScanSummary{Entries: st.count, IsComplete: false}, nil
	case errors.Is(bctx.Err(), context.DeadlineExceeded):
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	case st.decodeErr != nil:
		// Keep JSON decode failures distinct from restic process failures.
		return model.BrowseScanSummary{Entries: st.count}, &Error{Kind: KindParse, Op: "ls", wrapped: st.decodeErr}
	default:
		return model.BrowseScanSummary{Entries: st.count}, c.classify(bctx, "ls", runErr, stderr)
	}
}

// ErrNoStreamRunner indicates that a Client has neither Stream nor a Runner
// implementing StreamRunner.
var ErrNoStreamRunner = errors.New("resticx: no stream runner configured (set Client.Stream)")

// streamRunner uses the explicit Stream seam or a Runner implementing
// StreamRunner. It never silently replaces a buffered-only runner with ExecRunner.
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

// consume forwards filesystem nodes from an NDJSON stream. A callback error
// cancels the producer; decode errors are recorded for the caller to classify.
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
