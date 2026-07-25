// Command resticscope provides a terminal UI and CLI for monitoring and
// operating restic repositories through the internal app core. It supports any
// backend restic supports.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. It is the
// testable entry point: all I/O goes through the passed writers.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
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
	case "check":
		return cmdCheck(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "exec":
		return cmdExec(ctx, rest, stdout, stderr)
	case "cache":
		return cmdCache(ctx, rest, stdout, stderr)
	case "secrets":
		return cmdSecrets(ctx, rest, stdout, stderr)
	case app.ExtractHelperSubcommand:
		// Internal: the root side of a privileged extract, launched by the TUI
		// via `sudo -n -- <self> extract-helper`, request on stdin.
		return cmdExtractHelper(ctx, rest, stdout, stderr)
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
  resticscope check [--config PATH]                validate config, secrets, restic, and repo reachability
  resticscope exec [--config PATH] <repo>          open a shell scoped to <repo> (RESTIC_*/credentials preloaded)
  resticscope exec [--config PATH] <repo> -- cmd   run cmd in that environment instead of a shell
  resticscope cache prune [--config PATH]          remove restic caches for repos no longer in config
  resticscope cache prune --all                    remove every repo's restic cache (restic rebuilds it)
  resticscope cache prune --dry-run                report what would be removed, delete nothing
  resticscope secrets template [--config PATH]     print a blank secrets JSON skeleton for your config
  resticscope version                              print resticscope and restic versions

status exit codes: 0 all green, 1 any amber, 2 any red/error/grey (or a failure).
check  exit codes: 0 all passed, 1 problems found, 2 could not run the check.
exec   exit codes: the command's own exit code; 1 if it cannot be launched; 2 on setup failure.
`)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
