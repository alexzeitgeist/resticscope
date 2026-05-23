package secrets

import (
	"bytes"
	"context"
	"os/exec"
)

// ExecRunner is the production RunFunc: it executes `shell -c command`. The
// secrets_command is given full shell power by design (plan §3, §12), so the
// caller is responsible for resolving a trusted shell.
func ExecRunner(ctx context.Context, shell, command string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}
