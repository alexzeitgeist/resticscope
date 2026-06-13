package resticx

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

// streamStderrLimit caps how much restic stderr RunStream retains. A verbose
// restic error must neither fill the stderr pipe (which would block the stdout
// reader) nor balloon memory, so the drain goroutine stops storing past this.
const streamStderrLimit = 64 << 10

// ExecRunner is the production Runner. It executes the restic binary on PATH
// and delivers the repository password through a pipe on fd 3, which the
// environment references as RESTIC_PASSWORD_FILE=/dev/fd/3. This keeps the
// password out of the argument list and out of /proc/<pid>/environ (plan §7).
type ExecRunner struct{}

// Run executes restic with env and args, delivering password over the fd-3
// pipe, and returns its fully-buffered stdout, stderr, and exit error.
func (ExecRunner) Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "restic", args...)
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

// RunStream executes restic and hands onStdout a reader over its stdout, instead
// of buffering all output. It reuses the identical fd-3 password mechanism as
// Run (the password never touches env or args). To avoid a stderr-pipe deadlock,
// a goroutine drains stderr concurrently into a size-limited buffer while
// onStdout reads stdout on the calling goroutine. When onStdout returns, leftover
// stdout is drained so restic is never blocked on a full pipe, the process is
// waited for, and the captured stderr plus the resulting error are returned. The
// onStdout error takes precedence over the wait error: a deliberate cap-cancel
// surfaces through onStdout, and the caller classifies the wait error itself.
func (ExecRunner) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	return runStreamFDs(ctx, env, password, nil, onStdout, args...)
}

// RunStreamPatterns is RunStream with one more out-of-band payload: patterns is
// delivered to the child on fd 4 — the argv references it as /dev/fd/4 — via
// the same pipe mechanism the password uses on fd 3. Restore include patterns
// ride here so they touch neither argv (visible in /proc/<pid>/cmdline) nor
// the filesystem.
func (ExecRunner) RunStreamPatterns(ctx context.Context, env []string, password string, patterns []byte, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	return runStreamFDs(ctx, env, password, patterns, onStdout, args...)
}

// runStreamFDs is the shared core of RunStream / RunStreamPatterns. The
// password pipe always occupies the fd-3 slot whenever any extra fd is wired
// (even for an empty password) so the /dev/fd/4 reference in argv can never
// shift. Both payloads are written from goroutines: a patterns payload larger
// than the kernel pipe buffer would otherwise deadlock the spawn, and if the
// child exits without reading, the deferred close of the parent's read end
// EPIPEs the writer so the goroutine always terminates.
func runStreamFDs(ctx context.Context, env []string, password string, patterns []byte, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "restic", args...)
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
	// Drain any stdout the callback left unread so restic is never wedged on a
	// full pipe while we wait for it (e.g. after the callback hit a cap).
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	wg.Wait()

	if cbErr != nil {
		return errBuf.Bytes(), cbErr
	}
	return errBuf.Bytes(), waitErr
}

// LimitedBuffer is a bytes.Buffer that stops accepting data past Limit bytes
// (0 means unlimited), reporting every write as fully accepted so the writer
// never blocks. It caps the stderr captured by RunStream and by the app's
// privileged-helper runner — anywhere output from a hostile child process is
// kept around.
type LimitedBuffer struct {
	buf   bytes.Buffer
	Limit int64
}

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
