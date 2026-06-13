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

	"resticscope/internal/app"
	"resticscope/internal/cache"
)

// cmdExec resolves a repo's credentials and either drops the user into an
// interactive shell scoped to it (with the orientation banner) or runs a single
// command in that environment when one is given after `--`:
//
//	resticscope exec [--config PATH] <repo>
//	resticscope exec [--config PATH] <repo> -- restic snapshots --json
//
// It shares the headless shell-out core with the TUI (app.ShellSession), so the
// environment, password handling, and snapshot context are identical (plan §8,
// §9). In command mode it returns the child's exit code so it composes in
// scripts; the interactive shell returns 0 on a clean exit.
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
		// refreshDeps runs secrets_command and validates the resolved secrets;
		// its error is already secret-free.
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

// parseExecArgs splits the exec arguments into flags, the repo name, and an
// optional command following `--`. Everything after the first `--` is the
// command to run verbatim; the rest is parsed for flags and the positional repo.
// Exactly one positional repo may precede `--`: extra bare words are rejected
// (almost always a forgotten `--`) rather than silently dropped, since opening a
// shell for the first word and discarding the rest is hard to diagnose.
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
		// Extra positional args almost always mean a forgotten `--`: the user
		// typed `exec repo restic snapshots` meaning to run a command. Reject it
		// and show exactly how to express that intent rather than discarding the
		// tail and dropping into an interactive shell for the first word.
		extra := strings.Join(fs.Args()[1:], " ")
		fmt.Fprintf(errOut, "exec: unexpected arguments after repo %q: %s\n", fs.Arg(0), extra)
		fmt.Fprintf(errOut, "to run a command in the repo shell, separate it with --:\n  resticscope exec %s -- %s\n", fs.Arg(0), extra)
		return "", "", nil, false
	}
	return *cfg, fs.Arg(0), cmdArgs, true
}

// runExec launches the session. With no command it runs the interactive-shell
// wrapper (banner + exec $SHELL); with a command it runs that command directly
// in the session environment. The child is wired to the real terminal so an
// interactive shell behaves normally.
func runExec(ctx context.Context, sess *app.ShellSession, cmdArgs []string, stdout, stderr io.Writer) int {
	interactive := len(cmdArgs) == 0

	var cmd *exec.Cmd
	if interactive {
		// No context cancellation here: the shell owns the foreground and exits
		// on the user's command. Signals are handled below. The interactive env
		// additionally carries the prompt-tag scaffolding (zsh ZDOTDIR); command
		// mode below gets the plain credential env.
		ia := sess.InteractiveArgs()
		cmd = exec.Command(ia[0], ia[1:]...)
		cmd.Env = sess.InteractiveEnv()
	} else {
		cmd = exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
		cmd.Env = sess.Env
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if interactive {
		// Drain SIGINT because the child shell should handle Ctrl-C while it owns
		// the terminal; resticscope should wait for cmd.Run to return.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer func() {
			signal.Stop(sigCh)
			close(sigCh)
		}()
		go func() {
			for range sigCh {
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
