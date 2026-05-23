package resticx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

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
