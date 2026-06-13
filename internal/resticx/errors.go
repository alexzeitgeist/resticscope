package resticx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ErrorKind classifies a restic failure. The numeric exit codes come from
// restic 0.17.0+; older versions are not supported (plan §9, §12).
type ErrorKind int

// The ErrorKind values; KindUnknown is the zero value, and the inline comment
// on each gives the restic exit code or condition it classifies.
const (
	KindUnknown       ErrorKind = iota
	KindRepoNotFound            // restic exit 10
	KindLocked                  // restic exit 11
	KindWrongPassword           // restic exit 12
	KindBinaryMissing           // restic not found on PATH
	KindTimeout                 // context deadline exceeded
	KindParse                   // restic ran but its JSON could not be parsed
	KindCanceled                // the caller cancelled the context
	KindPartial                 // restic completed but some items failed (e.g. a partial restore)
)

// Error is a classified restic failure. Its message is a clear diagnostic
// rather than a raw stderr dump; Stderr is already redacted by the time it is
// stored here, so the error is always safe to log.
type Error struct {
	Kind    ErrorKind
	Op      string // restic subcommand, e.g. "snapshots"
	Code    int    // restic exit code, when known
	Stderr  string // redacted
	wrapped error
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindRepoNotFound:
		return fmt.Sprintf("restic %s: repository does not exist (exit 10)", e.Op)
	case KindLocked:
		return fmt.Sprintf("restic %s: repository is locked (exit 11)", e.Op)
	case KindWrongPassword:
		return fmt.Sprintf("restic %s: wrong password (exit 12)", e.Op)
	case KindBinaryMissing:
		return "restic binary not found on PATH"
	case KindTimeout:
		return fmt.Sprintf("restic %s: timed out", e.Op)
	case KindParse:
		return fmt.Sprintf("restic %s: could not parse JSON output: %v", e.Op, e.wrapped)
	case KindCanceled:
		return fmt.Sprintf("restic %s: canceled", e.Op)
	case KindPartial:
		return fmt.Sprintf("restic %s: completed with errors", e.Op)
	default:
		if e.Stderr != "" {
			return fmt.Sprintf("restic %s failed: %v (stderr: %s)", e.Op, e.wrapped, e.Stderr)
		}
		return fmt.Sprintf("restic %s failed: %v", e.Op, e.wrapped)
	}
}

func (e *Error) Unwrap() error { return e.wrapped }

// exitCoder is implemented by *exec.ExitError and by test fakes, so
// classification works without spawning a real process.
type exitCoder interface{ ExitCode() int }

func (c *Client) classify(ctx context.Context, op string, err error, stderr []byte) error {
	redacted := string(stderr)
	if c.Redact != nil {
		redacted = c.Redact(redacted)
	}

	if errors.Is(ctx.Err(), context.Canceled) {
		return &Error{Kind: KindCanceled, Op: op, wrapped: ctx.Err()}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Op: op, Stderr: redacted, wrapped: err}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &Error{Kind: KindBinaryMissing, Op: op, wrapped: err}
	}

	var ec exitCoder
	if errors.As(err, &ec) {
		code := ec.ExitCode()
		kind := KindUnknown
		switch code {
		case 10:
			kind = KindRepoNotFound
		case 11:
			kind = KindLocked
		case 12:
			kind = KindWrongPassword
		}
		return &Error{Kind: kind, Op: op, Code: code, Stderr: redacted, wrapped: err}
	}

	return &Error{Kind: KindUnknown, Op: op, Stderr: redacted, wrapped: err}
}
