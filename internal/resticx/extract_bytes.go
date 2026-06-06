package resticx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extract_bytes.go is the streaming restic boundary for extracting a single
// regular file's raw bytes out of a snapshot — `restic dump <snap> <source>`.
// It is the sibling of extract_tree.go: it assembles a safe argv (the §5 safety
// invariants this layer owns are encoded in buildExtractBytesArgs and asserted
// by argv tests), opens the staging target with O_CREATE|O_EXCL|O_WRONLY,
// streams restic's stdout through a counting reader into that file, and
// classifies the exit code into the package's existing *Error / ErrorKind shape.
//
// Unlike tree mode, resticscope owns the output file: no --target / --overwrite
// is handed to restic. The O_EXCL open is the refuse-if-exists guarantee, the
// byte count drives the progress UI, and a partial file on cancel/error is left
// in place — its removal is the app layer's keep-or-delete decision, not this
// layer's. The BrowseEntry.Type == "file" gate (rejecting symlinks/devices/
// fifos/sockets) lives upstream in step 04 where the node type is visible; this
// wrapper assumes the caller has already rejected non-regular sources.

// ErrExtractTargetExists is returned when the O_EXCL open of the per-file target
// reports EEXIST. It is path-free by construction (it never echoes the target)
// so a rejection can be logged safely; the TUI knows the target it just typed
// and can show it transiently.
var ErrExtractTargetExists = errors.New("resticx: extract target already exists")

// ExtractBytesParams are the inputs to a single restic dump invocation. The
// caller (app.Extract) builds Source/Target/SnapshotID and ensures the parent of
// Target (the staging dir) already exists; this layer creates only the file.
type ExtractBytesParams struct {
	// SnapshotID is the concrete lowercase-hex snapshot ID. Never "latest".
	SnapshotID string

	// Source is the cleaned, rooted file path inside the snapshot ("/etc/hosts").
	// It must already equal model.CleanBrowsePath(Source) and carry no NUL; this
	// layer asserts that and returns ErrExtractInvalidSource on violation rather
	// than re-cleaning. Unlike tree mode, "" and "/" (the whole-snapshot root) are
	// invalid here — bytes mode dumps exactly one file.
	Source string

	// Target is the absolute path of the file restic's stdout is copied into. The
	// parent must exist; the file itself must NOT (the wrapper opens it
	// O_CREATE|O_EXCL|O_WRONLY at mode 0600, umask still applies, and returns
	// ErrExtractTargetExists on EEXIST).
	Target string
}

// ExtractBytesProgress reports streaming progress from the copy loop. BytesTotal
// is intentionally absent: restic dump does not announce a content size up
// front, so step 05 renders "n B / —" until completion.
type ExtractBytesProgress struct {
	BytesDone int64
}

// ExtractBytesResult is returned from ExtractBytes. BytesWritten is populated on
// every path — success, cancel, and error — so the caller can render or log the
// bytes actually copied before an interruption.
type ExtractBytesResult struct {
	BytesWritten int64
}

// ExtractBytes streams `restic --no-lock dump <snap> <source>` and copies its
// stdout into params.Target, opened O_CREATE|O_EXCL|O_WRONLY. onProgress is
// best-effort and may be nil; it is called synchronously from the copy loop with
// the cumulative byte count, so it must be cheap and non-blocking — a slow
// callback stalls stdout consumption and therefore restic. Coalescing or
// sampling is the caller's job (00-framework.md §11).
//
// Like ExtractTree, this layer imposes no timeout — a large dump can outlast
// resticx's 2-minute default, so the deadline is the caller's (app.Extract
// bounds ctx with the configured extract_timeout). The child context exists only
// so the copy can stop restic when ctx is canceled.
func (c *Client) ExtractBytes(ctx context.Context, t Target, creds Creds, params ExtractBytesParams, onProgress func(ExtractBytesProgress)) (ExtractBytesResult, error) {
	args, err := buildExtractBytesArgs(params)
	if err != nil {
		return ExtractBytesResult{}, err // path-free validation sentinel; no file created, no restic spawned
	}

	// Open the target FIRST so an existing file (or a bad parent) fails before any
	// restic process is spawned — a privacy win (no process leak on a duplicate
	// target) and a perf win (no wasted restic startup).
	f, err := os.OpenFile(params.Target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ExtractBytesResult{}, mapOpenTargetError(err)
	}
	defer f.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	full := make([]string, 0, len(args)+2)
	if t.BucketLookup == "dns" || t.BucketLookup == "path" {
		full = append(full, "-o", "s3.bucket-lookup="+t.BucketLookup)
	}
	full = append(full, args...)

	env := c.buildEnv(t, creds)

	var written int64
	var copyErr error
	stderr, runErr := c.streamRunner().RunStream(runCtx, env, creds.ResticPassword, func(stdout io.Reader) error {
		cr := &countingReader{r: stdout, onProgress: onProgress}
		written, copyErr = io.Copy(f, cr)
		if copyErr != nil {
			// Stop restic promptly. Without this, RunStream drains the rest of the
			// dump's stdout before Wait, turning a local write failure (e.g. ENOSPC
			// partway through a large file) into a full remote read of the remainder.
			// Mirrors the tree/browse streams, which cancel on a callback error.
			cancel()
		}
		return copyErr
	}, full...)

	res := ExtractBytesResult{BytesWritten: written}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// A caller cancel beats every other classification; the partial bytes
		// already on disk stay there for the app layer to keep or delete.
		return res, context.Canceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, c.classify(ctx, "dump", runErr, c.sanitizeExtractBytesStderr(stderr, params))
	case copyErr != nil:
		// A local failure copying restic's stdout into the staging file (e.g.
		// ENOSPC) — its os.PathError form would echo the target path, so surface a
		// path-free wrapper rather than classifying the raw error.
		return res, pathFreeFSError("extract dump stream failed", copyErr)
	case runErr == nil:
		return res, nil
	default:
		return res, c.classify(ctx, "dump", runErr, c.sanitizeExtractBytesStderr(stderr, params))
	}
}

// buildExtractBytesArgs assembles the dump argv and is this layer's assertion
// surface for the safety invariants it owns (00-framework.md §5). It emits, in
// order: --no-lock (a lock would be a write), dump, the concrete snapshot ID,
// and the cleaned source file. It never emits --target, --overwrite, --dry-run,
// --path, --delete, or "latest" — resticscope owns the output file, so dump's
// own target-writing mode is deliberately unused. The bucket-lookup -o option is
// prepended by ExtractBytes, not here, so these argv tests stay free of S3 noise.
func buildExtractBytesArgs(p ExtractBytesParams) ([]string, error) {
	if err := assertCleanSnapshotID(p.SnapshotID); err != nil {
		return nil, err
	}
	if err := assertBytesSource(p.Source); err != nil {
		return nil, err
	}
	if p.Target == "" || !filepath.IsAbs(p.Target) {
		return nil, ErrExtractInvalidTarget
	}
	return []string{"--no-lock", "dump", p.SnapshotID, p.Source}, nil
}

// assertBytesSource is assertCleanSource (the shared cleaned-path contract) plus
// the bytes-mode rule that the whole-snapshot root ("" or "/") is not a dumpable
// file — bytes mode extracts exactly one regular file.
func assertBytesSource(src string) error {
	if src == "" || src == "/" {
		return ErrExtractInvalidSource
	}
	return assertCleanSource(src)
}

// sanitizeExtractBytesStderr prepares restic dump stderr to be safe inside a
// returned *Error by scrubbing the bytes-extract source/target fragments through
// the shared scrubExtractStderr.
func (c *Client) sanitizeExtractBytesStderr(stderr []byte, p ExtractBytesParams) []byte {
	return c.scrubExtractStderr(stderr, extractPathFragments(p.Source, p.Target))
}

// mapOpenTargetError converts the os.OpenFile error into a path-free error: a
// pre-existing target becomes the ErrExtractTargetExists sentinel, and any other
// failure (e.g. a missing or read-only parent) is wrapped with its filesystem
// path stripped so the privacy contract holds.
func mapOpenTargetError(err error) error {
	if errors.Is(err, os.ErrExist) {
		return ErrExtractTargetExists
	}
	return pathFreeFSError("cannot create extract target", err)
}

// pathFreeFSError strips the filesystem path from an *os.PathError so a local IO
// failure can be surfaced without leaking the source/target path (privacy
// contract, 00-framework.md §3). Non-PathError causes carry no path and are
// wrapped verbatim.
func pathFreeFSError(label string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("resticx: %s: %w", label, err)
}

// countingReader wraps restic's stdout and reports the cumulative byte count to
// onProgress after each non-empty read. The callback runs synchronously on the
// copy goroutine — it must be cheap and non-blocking or it stalls the read (see
// ExtractBytes). It is allocation-free in the hot path: the progress value is a
// small struct passed by value, with no per-read closure capture. onProgress may
// be nil.
type countingReader struct {
	r          io.Reader
	n          int64
	onProgress func(ExtractBytesProgress)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.n += int64(n)
		if cr.onProgress != nil {
			cr.onProgress(ExtractBytesProgress{BytesDone: cr.n})
		}
	}
	return n, err
}
