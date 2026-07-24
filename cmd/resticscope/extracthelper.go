package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/alexzeitgeist/resticscope/internal/app"
)

// cmdExtractHelper is the hidden subcommand behind the TUI's privileged
// extract. The TUI re-execs this binary as `sudo -n -- <self> extract-helper`;
// the request (repo target, resolved credentials, extract request) arrives as
// one JSON value on stdin and progress/result/error events leave as NDJSON on
// stdout. It is not meant to be run by hand: without the payload it just
// reports a bad request. All real work lives in app.RunExtractHelper; this
// shim only collects the process facts the app layer must not read globally
// (rule 4): effective uid, the invoking user from sudo's environment, and the
// clock.
func cmdExtractHelper(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "extract-helper: takes no arguments (internal command, driven over stdin)")
		return 2
	}
	opts := app.ExtractHelperOpts{
		Euid:     os.Geteuid(),
		OwnerUID: sudoEnvID("SUDO_UID"),
		OwnerGID: sudoEnvID("SUDO_GID"),
		Clock:    realClock{},
	}
	if err := app.RunExtractHelper(ctx, os.Stdin, stdout, opts); err != nil {
		// The error is path-free by RunExtractHelper's contract, and the parent
		// already received it as an error event; stderr is for sudo logs and
		// hand-runs.
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// sudoEnvID parses an id sudo placed in the environment (SUDO_UID/SUDO_GID).
// -1 when absent or malformed, which disables the helper's scaffolding chowns
// rather than guessing an owner.
func sudoEnvID(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
