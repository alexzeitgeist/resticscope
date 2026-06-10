package app

// extract_privileged_exec.go is the production PrivilegedRunner: the one place
// that knows how to launch the extract helper under sudo (rule 5 — a wrapped
// hostile boundary, deliberately isolated like resticx/exec.go). Everything
// here is shaped by one asymmetry: the child runs as root, so this process
// cannot signal it. The helper's stdin pipe is therefore the only control
// channel — the payload goes down it, and CLOSING it is the cancel/kill
// switch (the helper exits on stdin EOF). Always `sudo -n`: the run must fail
// fast rather than deadlock on an invisible password prompt; interactive
// authentication is the TUI's job (sudo -v via tea.ExecProcess) before Run.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// sudoProbeTimeout bounds `sudo -n true`. Generous: a misconfigured sudo
	// with LDAP/NSS lookups can stall for seconds, and the probe runs off the
	// UI thread.
	sudoProbeTimeout = 10 * time.Second

	// helperWaitDelay bounds Wait after the stdin pipe closes. We cannot
	// SIGKILL a root child, so if the helper ignores EOF (a bug), os/exec
	// closes our ends of the pipes and Wait returns ErrWaitDelay instead of
	// hanging the extract Cmd goroutine forever.
	helperWaitDelay = 15 * time.Second

	// helperStderrLimit caps captured helper/sudo stderr (it is path-free by
	// the helper's contract, but unbounded capture is never acceptable).
	helperStderrLimit = 32 << 10

	// helperEventLineMax caps one stdout event line. Events are small JSON
	// records; 1 MiB is far above any legitimate line.
	helperEventLineMax = 1 << 20

	// helperSubcommand is the hidden cmd/resticscope subcommand the runner
	// re-execs. Kept here, next to the argv assembly, so runner and cmd wiring
	// share one constant.
	helperSubcommand = "extract-helper"
)

// SudoPrivilegedRunner launches `sudo -n -- <Exe> extract-helper`. Exe is the
// absolute path to the running resticscope binary: sudo resets PATH
// (secure_path), so the helper must be addressed absolutely, and re-execing
// self guarantees parent and helper agree on the wire format.
type SudoPrivilegedRunner struct {
	Exe string
}

// NewSudoPrivilegedRunner resolves the running binary. It fails only when the
// executable path cannot be determined, in which case the caller leaves
// App.Priv nil and privileged extracts are reported unavailable.
func NewSudoPrivilegedRunner() (*SudoPrivilegedRunner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable: %w", err)
	}
	return &SudoPrivilegedRunner{Exe: exe}, nil
}

// Probe runs `sudo -n true`. nil means a helper launch will not prompt
// (cached credentials or NOPASSWD); an error carries sudo's first stderr line
// (path-free: sudo names no repo or target paths) so the TUI can decide to
// authenticate interactively.
func (r *SudoPrivilegedRunner) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sudoProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "true")
	// No stdin: a misconfigured sudo must fail, never read the terminal.
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if line := firstNonEmptyLine(out); line != "" {
		return fmt.Errorf("sudo: %s", line)
	}
	return fmt.Errorf("sudo -n: %w", err)
}

// Run launches the helper, writes payload to its stdin (keeping the pipe open
// as the liveness channel), and streams stdout lines to onLine. See the
// package comment for the control-channel design; the error path returns the
// first line of captured stderr, which the helper keeps path-free.
func (r *SudoPrivilegedRunner) Run(ctx context.Context, payload []byte, onLine func(line []byte) error) error {
	cmd := exec.CommandContext(ctx, "sudo", "-n", "--", r.Exe, helperSubcommand)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr cappedBuffer
	stderr.limit = helperStderrLimit
	cmd.Stderr = &stderr

	// ctx cancellation closes stdin instead of the default SIGKILL, which
	// would fail with EPERM against a root child. WaitDelay then guarantees
	// Wait returns even if the helper ignores the EOF.
	cmd.Cancel = func() error { return stdin.Close() }
	cmd.WaitDelay = helperWaitDelay

	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		// Deliver the payload; deliberately do NOT close stdin — the open pipe
		// is what tells the helper the parent is alive. cmd.Cancel (ctx) or the
		// deferred close below ends it.
		_, _ = stdin.Write(payload)
	}()
	defer func() { _ = stdin.Close() }()

	var cbErr error
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), helperEventLineMax)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if cbErr = onLine(line); cbErr != nil {
			break
		}
	}
	// Drain leftover stdout so the helper is never wedged on a full pipe
	// while we wait for it (mirrors resticx.ExecRunner.RunStream).
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if cbErr != nil {
		return cbErr
	}
	if scanErr := sc.Err(); scanErr != nil {
		return fmt.Errorf("helper output: %w", scanErr)
	}
	if waitErr != nil {
		if line := firstNonEmptyLine(stderr.Bytes()); line != "" {
			return fmt.Errorf("%w: %s", waitErr, line)
		}
		return waitErr
	}
	return nil
}

// firstNonEmptyLine returns the first non-blank, space-trimmed line of b.
func firstNonEmptyLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// cappedBuffer accepts writes up to limit bytes and silently discards the
// rest, reporting full acceptance so the writer never blocks. Local sibling of
// resticx's limitedBuffer (unexported there).
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	accepted := len(p)
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return accepted, nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
