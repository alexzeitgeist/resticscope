package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/alexzeitgeist/resticscope/internal/app"
)

// cmdExtractHelper is the hidden privileged-extract subprocess invoked as
// `sudo -n -- <self> extract-helper`. It reads one JSON request, including
// credentials, from stdin and emits NDJSON events on stdout. app.RunExtractHelper
// owns the workflow; this shim supplies process identity and time.
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
		// RunExtractHelper emits path-free errors as events when possible; stderr
		// is reserved for sudo logs and manual runs.
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// sudoEnvID parses an ID placed in the environment by sudo. It returns -1 for
// absent or malformed values so the helper disables chown instead of guessing.
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
