package resticx

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

// streamStderrLimit bounds retained stderr without blocking its pipe or growing
// memory with hostile output.
const streamStderrLimit = 64 << 10

// ExecRunner executes restic from PATH. It supplies the repository password on
// fd 3 through RESTIC_PASSWORD_FILE=/dev/fd/3, keeping the password out of argv
// and the process environment.
type ExecRunner struct{}

// Run executes restic with env and args, delivering password over the fd-3
// pipe, and returns its fully-buffered stdout, stderr, and exit error.
func (ExecRunner) Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "restic", args...) //nolint:gosec // G204: the executable is the literal "restic"; running it with computed arguments is this package's purpose, and no shell is involved
	cmd.Env = env

	if password != "" {
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, nil, pipeErr
		}
		defer func() { _ = pr.Close() }()
		// pr becomes fd 3 in the child (fds 0,1,2 are stdio).
		cmd.ExtraFiles = []*os.File{pr}
		go func() {
			_, _ = pw.WriteString(password)
			_ = pw.Close()
		}()
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// RunStream passes restic's stdout to onStdout and captures bounded stderr. It
// uses Run's fd-3 password mechanism, and callback errors take precedence over
// process wait errors.
func (ExecRunner) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	return runStreamFDs(ctx, env, password, nil, onStdout, args...)
}

// RunStreamPatterns additionally supplies patterns on fd 4, referenced in argv
// as /dev/fd/4. Pattern contents appear in neither argv nor the filesystem.
func (ExecRunner) RunStreamPatterns(ctx context.Context, env []string, password string, patterns []byte, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	return runStreamFDs(ctx, env, password, patterns, onStdout, args...)
}

// runStreamFDs reserves fd 3 whenever an extra descriptor is needed, preventing
// the fd-4 pattern reference from shifting. Goroutine writers avoid pipe-buffer
// deadlocks during startup; closing the read ends releases them if the child
// exits without consuming a payload.
func runStreamFDs(ctx context.Context, env []string, password string, patterns []byte, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "restic", args...) //nolint:gosec // G204: see ExecRunner.Run; the executable is a literal and no shell is involved
	cmd.Env = env

	if password != "" || patterns != nil { //nolint:nestif // sequential out-of-band fd wiring: the outer block sets up the fd-3 password pipe, the inner branch only adds the optional fd-4 patterns pipe
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		defer func() { _ = pr.Close() }()
		cmd.ExtraFiles = []*os.File{pr} // pr becomes fd 3 in the child
		go func() {
			if password != "" {
				_, _ = pw.WriteString(password)
			}
			_ = pw.Close()
		}()
		if patterns != nil {
			pr4, pw4, pipeErr := os.Pipe()
			if pipeErr != nil {
				return nil, pipeErr
			}
			defer func() { _ = pr4.Close() }()
			cmd.ExtraFiles = append(cmd.ExtraFiles, pr4) // pr4 becomes fd 4
			go func() {
				_, _ = pw4.Write(patterns)
				_ = pw4.Close()
			}()
		}
	}

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return nil, pipeErr
	}
	errPipe, pipeErr := cmd.StderrPipe()
	if pipeErr != nil {
		return nil, pipeErr
	}

	if startErr := cmd.Start(); startErr != nil {
		return nil, startErr
	}

	var errBuf LimitedBuffer
	errBuf.Limit = streamStderrLimit
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(&errBuf, errPipe)
	})

	cbErr := onStdout(stdout)
	// Drain unread stdout so waiting cannot deadlock on a full child pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	wg.Wait()

	if cbErr != nil {
		return errBuf.Bytes(), cbErr
	}
	return errBuf.Bytes(), waitErr
}

// LimitedBuffer retains at most Limit bytes, or unlimited bytes when Limit is
// zero. Writes report full acceptance so hostile child output cannot block the
// stderr drain.
type LimitedBuffer struct {
	buf   bytes.Buffer
	Limit int64
}

// Write retains up to Limit bytes from p and always reports len(p) accepted.
func (b *LimitedBuffer) Write(p []byte) (int, error) {
	accepted := len(p)
	if b.Limit > 0 {
		remaining := b.Limit - int64(b.buf.Len())
		if remaining <= 0 {
			return accepted, nil
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	_, _ = b.buf.Write(p)
	return accepted, nil
}

// Bytes returns the accumulated (capped) contents.
func (b *LimitedBuffer) Bytes() []byte { return b.buf.Bytes() }
