package resticx

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Snapshot diffs use one no-lock JSON stream. Callback errors abort the scan,
// while caller cancellation takes precedence over other outcomes.

// StreamDiff sends each parsed change to onEntry and coalesced counts to
// onProgress. The diff is directional: `+` means present only in the second
// snapshot, while `-` means present only in the first. restic reports `U`
// (metadata-only) changes only when metadata is true.
func (c *Client) StreamDiff(ctx context.Context, t Target, creds Creds, olderID, newerID string, metadata bool, timeout time.Duration, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error) {
	if timeout <= 0 {
		timeout = c.timeout()
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--no-lock", "diff", "--json"}
	if metadata {
		args = append(args, "--metadata")
	}
	full := prependBackendOpts(t, append(args, olderID, newerID)...)

	runner, err := c.streamRunner()
	if err != nil {
		return model.SnapshotDiff{}, err
	}

	env := c.buildEnv(t, creds)
	st := &diffStream{ctx: dctx, onEntry: onEntry, onProgress: onProgress}
	stderr, runErr := runner.RunStream(dctx, env, creds.ResticPassword, st.consume, full...)

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// The parser may also have surfaced the cancellation through scanErr.
		return st.result, context.Canceled
	case errors.Is(dctx.Err(), context.DeadlineExceeded):
		return st.result, c.classify(dctx, "diff", runErr, stderr)
	case st.scanErr != nil && !errors.Is(st.scanErr, context.Canceled):
		// onEntry or scanner failure (e.g. line over the buffer cap, store error).
		return st.result, st.scanErr
	case runErr == nil:
		return st.result, nil
	default:
		return st.result, c.classify(dctx, "diff", runErr, stderr)
	}
}

// diffStream retains the terminal scan result and first non-cancellation error.
type diffStream struct {
	ctx        context.Context
	onEntry    func(model.DiffEntry) error
	onProgress func(seen int)

	result  model.SnapshotDiff
	scanErr error
}

func (s *diffStream) consume(r io.Reader) error {
	res, err := model.ScanDiffNDJSON(s.ctx, r, s.onEntry, s.onProgress)
	s.result = res
	if err != nil {
		s.scanErr = err
		return err
	}
	return nil
}
