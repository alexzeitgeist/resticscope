package app

// The production privileged runner launches the root helper with non-interactive
// sudo. Because the parent cannot signal the root child, stdin carries the
// payload and EOF cancellation. Interactive sudo authentication occurs in the
// TUI before Run.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

const (
	// sudoProbeTimeout bounds slow sudo credential and identity lookups.
	sudoProbeTimeout = 10 * time.Second

	// helperWaitDelay is os/exec's grace period after context cancellation.
	helperWaitDelay = 15 * time.Second

	// helperStderrLimit bounds captured helper and sudo stderr.
	helperStderrLimit = 32 << 10

	// helperEventLineMax bounds one JSON event line.
	helperEventLineMax = 1 << 20

	// ExtractHelperSubcommand is the hidden self-reexec command.
	ExtractHelperSubcommand = "extract-helper"
)

// SudoPrivilegedRunner launches Exe through non-interactive sudo. The
// constructor supplies an absolute executable path, avoiding PATH lookup.
type SudoPrivilegedRunner struct {
	Exe string
}

// NewSudoPrivilegedRunner resolves the running binary. Failure leaves
// privileged extraction unavailable to the caller.
func NewSudoPrivilegedRunner() (*SudoPrivilegedRunner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable: %w", err)
	}
	return &SudoPrivilegedRunner{Exe: exe}, nil
}

// Probe performs a best-effort non-interactive sudo check. Errors include
// sudo's first stderr line so the TUI can request authentication.
func (r *SudoPrivilegedRunner) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sudoProbeTimeout)
	defer cancel()
	// Command-specific sudoers policy may differ from this best-effort probe.
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

// AuthCommand returns an interactive sudo validation command. Its path-free
// prompt explains the request while sudo owns the terminal, and success warms
// the cache used by Run.
func (r *SudoPrivilegedRunner) AuthCommand() *exec.Cmd {
	return exec.Command("sudo", "-v",
		"-p", "resticscope needs sudo to extract as root (preserves snapshot ownership): ")
}

// Run sends the payload while keeping stdin open as the liveness channel and
// streams stdout events to onLine. Errors may include the first captured
// stderr line.
func (r *SudoPrivilegedRunner) Run(ctx context.Context, payload []byte, onLine func(line []byte) error) error {
	cmd := exec.CommandContext(ctx, "sudo", "-n", "--", r.Exe, ExtractHelperSubcommand) //nolint:gosec // fixed argv: sudo re-execs this same binary's helper subcommand, no shell

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr resticx.LimitedBuffer
	stderr.Limit = helperStderrLimit
	cmd.Stderr = &stderr

	// Cancellation closes stdin so the helper sees EOF across the privilege boundary.
	// WaitDelay lets it exit before os/exec attempts to kill sudo; Once makes
	// concurrent closes share the same result.
	closeStdin := sync.OnceValue(func() error { return stdin.Close() })
	cmd.Cancel = closeStdin
	cmd.WaitDelay = helperWaitDelay

	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		// Keep stdin open after the payload as the parent's liveness signal.
		_, _ = stdin.Write(payload)
	}()

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
	// Close stdin before drain and Wait so callback failure stops work whose
	// result the parent has discarded.
	_ = closeStdin()
	// Drain stdout so a full pipe cannot block helper exit.
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

func firstNonEmptyLine(b []byte) string {
	for line := range strings.SplitSeq(string(b), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
