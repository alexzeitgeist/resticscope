// Command resticscope is a local, on-demand overview of restic repositories on
// S3-compatible storage.
//
// Phase 0 ships the headless `status` and `version` commands; the Bubble Tea
// TUI and the `check`/`exec` subcommands are built on the same internal/app
// core in later phases.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. It is the
// testable entry point: all I/O goes through the passed writers.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// Bare `resticscope` launches the TUI (plan §9).
		return cmdTUI(ctx, nil, stdout, stderr)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "tui":
		return cmdTUI(ctx, rest, stdout, stderr)
	case "status":
		return cmdStatus(ctx, rest, stdout, stderr)
	case "version":
		return cmdVersion(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "check", "exec":
		fmt.Fprintf(stderr, "resticscope %s: not implemented yet (planned for a later phase)\n", cmd)
		return 2
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `resticscope — overview of restic repositories

Usage:
  resticscope [tui] [--config PATH]                launch the interactive TUI (default)
  resticscope status [--config PATH] [--refresh]   one line per repo from cache
  resticscope version                              print resticscope and restic versions

status exit codes: 0 all green, 1 any amber, 2 any red/error/grey (or a failure).

The check and exec subcommands are planned but not yet implemented.
`)
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
