package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/cache"
)

// cmdExec resolves repository credentials and either opens an interactive shell
// or runs the command following --. It shares app.ShellSession with the TUI, so
// credential environment and password handling match. CLI sessions are always
// repository-scoped; TUI sessions may also include a selected snapshot. Command
// mode returns the child's exit code, while a clean interactive exit returns 0.
func cmdExec(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfgPath, repoName, cmdArgs, ok := parseExecArgs(args, stderr)
	if !ok {
		return 2
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	logger := newLogger(cfg)
	store, _, err := refreshDeps(ctx, cfg, logger)
	if err != nil {
		// Command failures suppress provider output, and validation errors omit
		// values; malformed JSON errors may still quote one input byte.
		fmt.Fprintf(stderr, "exec: %v\n", err)
		return 2
	}

	a := &app.App{
		Cfg:     cfg,
		Cache:   cache.New(cfg.Global.CacheDir),
		Clock:   realClock{},
		Log:     logger,
		Secrets: store,
	}

	sess, err := a.ShellSession(repoName, nil)
	if err != nil {
		fmt.Fprintf(stderr, "exec: %v\n", err)
		return 2
	}
	defer sess.Cleanup() //nolint:errcheck // best-effort: this one-shot subcommand has no UI to report a cleanup failure on (the TUI, which is long-lived, does surface it)

	return runExec(ctx, sess, cmdArgs, stdout, stderr)
}

// parseExecArgs separates flags and one repository name from the command after
// the first --. It rejects extra positional arguments before the separator.
func parseExecArgs(args []string, errOut io.Writer) (cfgPath, repo string, cmdArgs []string, ok bool) {
	if i := slices.Index(args, "--"); i >= 0 {
		cmdArgs = args[i+1:]
		args = args[:i]
	}

	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfg := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	if err := fs.Parse(args); err != nil {
		return "", "", nil, false
	}
	switch {
	case fs.NArg() == 0:
		fmt.Fprintln(errOut, "usage: resticscope exec [--config PATH] <repo> [-- command args...]")
		return "", "", nil, false
	case fs.NArg() > 1:
		// Reject a likely missing -- instead of silently opening an interactive
		// shell for the first positional argument.
		extra := strings.Join(fs.Args()[1:], " ")
		fmt.Fprintf(errOut, "exec: unexpected arguments after repo %q: %s\n", fs.Arg(0), extra)
		fmt.Fprintf(errOut, "to run a command in the repo shell, separate it with --:\n  resticscope exec %s -- %s\n", fs.Arg(0), extra)
		return "", "", nil, false
	}
	return *cfg, fs.Arg(0), cmdArgs, true
}

// runExec launches the interactive-shell wrapper or runs a command directly in
// the session environment.
func runExec(ctx context.Context, sess *app.ShellSession, cmdArgs []string, stdout, stderr io.Writer) int {
	interactive := len(cmdArgs) == 0

	var cmd *exec.Cmd
	if interactive {
		// The foreground shell owns termination, so context cancellation is not
		// attached. Interactive mode also receives prompt scaffolding that command
		// mode does not.
		ia := sess.InteractiveArgs()
		cmd = exec.Command(ia[0], ia[1:]...) //nolint:gosec // args are the internally-built shell invocation; exec.Command runs no shell, so there is no injection vector
		cmd.Env = sess.InteractiveEnv()
	} else {
		cmd = exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...) //nolint:gosec // cmdArgs is the user-requested command (everything after --), run directly via exec.CommandContext with no shell expansion
		cmd.Env = sess.Env
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if interactive {
		// Drain SIGINT while the child owns the terminal; cmd.Run determines when
		// resticscope returns.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer func() {
			signal.Stop(sigCh)
			close(sigCh)
		}()
		go func() {
			for range sigCh { //nolint:revive // intentional drain: swallow SIGINT while the child shell owns the terminal
			}
		}()
	}

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "exec: %v\n", err)
		return 1
	}
	return 0
}
