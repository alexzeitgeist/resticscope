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

func (ExecRunner) Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = env

	if password != "" {
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, nil, pipeErr
		}
		defer pr.Close()
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
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = env

	if password != "" {
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		defer pr.Close()
		cmd.ExtraFiles = []*os.File{pr} // pr becomes fd 3 in the child
		go func() {
			_, _ = pw.WriteString(password)
			_ = pw.Close()
		}()
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

	var errBuf limitedBuffer
	errBuf.limit = streamStderrLimit
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, errPipe)
	}()

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

// limitedBuffer is a bytes.Buffer that stops accepting data past limit bytes,
// reporting every write as fully accepted so the writer never blocks. It caps
// the stderr captured by RunStream.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	accepted := len(p)
	if b.limit > 0 {
		remaining := b.limit - int64(b.buf.Len())
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

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
