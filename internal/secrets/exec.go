package secrets

import (
	"bytes"
	"context"
	"os/exec"
)

// ExecRunner executes command through `shell -c`. The caller must supply a
// trusted shell because secrets commands intentionally have full shell access.
func ExecRunner(ctx context.Context, shell, command string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}
