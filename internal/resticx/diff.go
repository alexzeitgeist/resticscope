package resticx

import (
	"context"
	"errors"
	"io"
	"time"

	"resticscope/internal/model"
)

// diff.go is the streaming restic boundary for the snapshot-diff feature. It
// runs `restic --no-lock diff --json <first> <second>` once and parses the
// NDJSON output as it arrives, handing each decoded change to onEntry. The
// boundary mirrors StreamSnapshotTree: callback errors abort the scan, user
// cancel beats every other classification, and a clean exit returns the
// summary verbatim.

// StreamDiff streams the change records for the diff between two snapshots,
// invoking onEntry for each parsed entry as it arrives and onProgress with a
// coalesced count. Restic diff is directional: `+` means present in the second
// argument and absent in the first, and `-` means the reverse. The TUI opens a
// chronological diff by default, then its swap key can pass the reverse order
// explicitly.
//
// Classification is ordered:
//  1. caller cancelled the parent ctx → context.Canceled, even if the parser
//     surfaced a cancel-equivalent error from the read.
//  2. onEntry / parser returned a non-cancel error → that error verbatim.
//  3. restic exited cleanly → the parsed SnapshotDiff, nil.
//  4. anything else → classify the run failure.
func (c *Client) StreamDiff(ctx context.Context, t Target, creds Creds, olderID, newerID string, timeout time.Duration, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error) {
	if timeout <= 0 {
		timeout = c.timeout()
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := make([]string, 0, 8)
	if t.BucketLookup == "dns" || t.BucketLookup == "path" {
		full = append(full, "-o", "s3.bucket-lookup="+t.BucketLookup)
	}
	full = append(full, "--no-lock", "diff", "--json", olderID, newerID)

	env := c.buildEnv(t, creds)
	st := &diffStream{ctx: dctx, onEntry: onEntry, onProgress: onProgress}
	stderr, runErr := c.streamRunner().RunStream(dctx, env, creds.ResticPassword, st.consume, full...)

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// A caller cancel beats every other classification — the parser may have
		// surfaced ctx.Err() up through scanErr.
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

// diffStream wires the consume callback to model.ScanDiffNDJSON and holds the
// terminal result plus the first non-cancel parser error.
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
