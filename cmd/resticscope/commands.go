package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"

	"resticscope/internal/app"
	"resticscope/internal/cache"
	"resticscope/internal/config"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
	"resticscope/internal/tui"
	"resticscope/internal/version"
)

func cmdVersion(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "resticscope %s\n", version.String())
	client := &resticx.Client{Runner: resticx.ExecRunner{}}
	if v, err := client.Version(ctx); err == nil {
		fmt.Fprintf(stdout, "restic %s\n", v)
	} else {
		fmt.Fprintln(stdout, "restic not found on PATH")
	}
	return 0
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	refresh := fs.Bool("refresh", false, "refresh from S3/restic before printing (slow; hits the network)")
	coverage := fs.Bool("coverage", false, "also print a cross-repo coverage rollup (missing hosts/paths/tags, stale repos)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	logger := newLogger(cfg)
	a := &app.App{
		Cfg:   cfg,
		Cache: cache.New(cfg.Global.CacheDir),
		Clock: realClock{},
		Log:   logger,
	}

	if *refresh {
		store, client, err := refreshDeps(ctx, cfg, logger)
		if err != nil {
			fmt.Fprintf(stderr, "refresh setup failed: %v\n", err)
			return 2
		}
		a.Secrets = store
		a.Restic = client
		states, err := a.RefreshAll(ctx)
		if err != nil {
			// The live states are still valid. A persistence failure must not
			// silently downgrade us to stale cache, so warn and render the
			// fresh results directly; the exit code reflects live health.
			fmt.Fprintf(stderr, "refresh: %v\n", err)
		}
		rows := a.RowsFromStates(states)
		formatStatusTable(stdout, rows, a.Clock.Now())
		if *coverage {
			fmt.Fprintln(stdout)
			formatCoverageRollup(stdout, app.Rollup(rows))
		}
		return refreshExitCode(rows, err)
	}

	rows, err := a.Statuses(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	formatStatusTable(stdout, rows, a.Clock.Now())
	if *coverage {
		fmt.Fprintln(stdout)
		formatCoverageRollup(stdout, app.Rollup(rows))
	}
	return app.WorstExitCode(rows)
}

// cmdTUI launches the Bubble Tea list view. It runs secrets_command and wires
// restic up front — before tea.NewProgram — so any GPG passphrase prompt
// happens at the normal terminal instead of fighting the alt-screen (plan §12).
// If secrets cannot be resolved, the TUI cannot refresh, so we fail fast with a
// clear message rather than launching a screen that can only show stale cache;
// `resticscope status` covers the cache-only, no-secrets case.
func cmdTUI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	logger := newLogger(cfg)
	store, client, err := refreshDeps(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(stderr, "startup failed: %v\n", err)
		return 2
	}

	a := &app.App{
		Cfg:     cfg,
		Cache:   cache.New(cfg.Global.CacheDir),
		Clock:   realClock{},
		Log:     logger,
		Secrets: store,
		Restic:  client,
	}

	var resticVer string
	if v, err := (&resticx.Client{Runner: resticx.ExecRunner{}}).Version(ctx); err == nil {
		resticVer = v
	}

	if err := tui.Run(ctx, a, resticVer); err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	return 0
}

// cmdCheck validates the wiring end to end: it loads and validates the config,
// runs secrets_command and confirms every credential and repo resolves, checks
// that restic meets the minimum supported version, then reaches each repo with
// `restic cat config`. It is the one-shot "is everything set up correctly?"
// command (plan §9, recommendation 3) and never reads or writes the cache.
//
// Exit codes: 0 everything passed; 1 the check ran but found problems (restic
// too old or unparseable, or one or more repos unreachable); 2 the check could
// not run or complete (bad config, secrets unavailable, no usable restic
// binary, or the run was interrupted).
func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		checkLine(stdout, "config", "FAILED", err.Error())
		return 2
	}
	checkLine(stdout, "config", "ok", fmt.Sprintf("%d repos, %d credentials", len(cfg.Repos), len(cfg.Credentials)))

	logger := newLogger(cfg)
	store, client, err := refreshDeps(ctx, cfg, logger)
	if err != nil {
		// refreshDeps runs secrets_command and validates the resolved secrets
		// against config; its error is already secret-free.
		checkLine(stdout, "secrets", "FAILED", err.Error())
		return 2
	}
	checkLine(stdout, "secrets", "ok", "all credentials and repos resolved")

	a := &app.App{
		Cfg:     cfg,
		Cache:   cache.New(cfg.Global.CacheDir),
		Clock:   realClock{},
		Log:     logger,
		Secrets: store,
		Restic:  client,
	}
	verClient := &resticx.Client{Runner: resticx.ExecRunner{}}
	return checkRestic(ctx, stdout, stderr, verClient.Version, a.Check)
}

// checkRestic runs the restic-version gate and the per-repo reachability stages
// of `check`, printing each and returning the exit code. The version is a hard
// gate: against an unsupported or unparseable restic, exit-code and JSON
// behavior are unreliable (see resticx.MinVersion), so the probe results would
// be untrustworthy — it prints the failed stage and returns without reaching
// any repository. A non-nil probe error (e.g. the context was cancelled) means
// the check could not complete, so the partial verdict is not trusted and the
// stage fails with exit 2 rather than risk reporting success.
//
// version and probe are injected so the gating and cancellation paths are
// testable without spawning a real restic.
func checkRestic(
	ctx context.Context,
	stdout, stderr io.Writer,
	version func(context.Context) (string, error),
	probe func(context.Context) ([]app.RepoCheck, error),
) int {
	ver, err := version(ctx)
	if err != nil {
		// No usable restic binary; probing would only repeat this per repo.
		checkLine(stdout, "restic", "FAILED", err.Error())
		return 2
	}
	switch ok, perr := resticx.AtLeastMinVersion(ver); {
	case perr != nil:
		checkLine(stdout, "restic", "FAILED", fmt.Sprintf("could not parse version %q: %v", ver, perr))
		fmt.Fprintln(stderr, "check failed: restic version unsupported")
		return 1
	case !ok:
		checkLine(stdout, "restic", "FAILED", fmt.Sprintf("%s is older than the minimum supported %s", ver, resticx.MinVersion))
		fmt.Fprintln(stderr, "check failed: restic version unsupported")
		return 1
	default:
		checkLine(stdout, "restic", "ok", ver)
	}

	checks, err := probe(ctx)
	if err != nil {
		// Cancelled or otherwise unable to finish: the per-repo rows are not a
		// trustworthy verdict, so report the stage as unfinished rather than
		// mistake incomplete probing for success.
		checkLine(stdout, "repositories", "FAILED", err.Error())
		fmt.Fprintf(stderr, "check failed: %v\n", err)
		return 2
	}

	fmt.Fprintln(stdout, "repositories")
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	failures := 0
	for _, rc := range checks {
		if rc.OK() {
			fmt.Fprintf(tw, "  %s\tok\n", rc.Name)
			continue
		}
		failures++
		fmt.Fprintf(tw, "  %s\tFAILED: %s\n", rc.Name, firstLine(rc.Err.Error()))
	}
	tw.Flush()

	if failures > 0 {
		fmt.Fprintf(stderr, "\ncheck failed: %d of %d repositories unreachable\n", failures, len(checks))
		return 1
	}
	fmt.Fprintln(stdout, "\nall checks passed")
	return 0
}

// refreshExitCode maps the live rows plus any refresh-level failure to a process
// exit code. A refresh failure (e.g. the cache could not be persisted) floors
// the code at 2 even when every repo is green, honoring the documented
// "...or a failure" contract: a successful-looking refresh whose result could
// not be saved must not exit 0 and mislead cron/shell callers.
func refreshExitCode(rows []app.RepoStatus, refreshErr error) int {
	code := app.WorstExitCode(rows)
	if refreshErr != nil && code < 2 {
		code = 2
	}
	return code
}

// refreshDeps runs the secrets_command, validates the resolved secrets against
// config, and constructs a restic client wired with a redactor so no secret can
// reach a log line or error string.
func refreshDeps(ctx context.Context, cfg *config.Config, logger *slog.Logger) (app.Secrets, app.Restic, error) {
	shell := cfg.Global.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}

	sctx, cancel := context.WithTimeout(ctx, cfg.Global.SecretsCommandTimeout.Std())
	defer cancel()
	store, err := secrets.Load(sctx, secrets.ExecRunner, shell, cfg.Global.SecretsCommand)
	if err != nil {
		return nil, nil, err
	}

	warnings, err := store.Validate(credentialNames(cfg), repoNames(cfg))
	for _, w := range warnings {
		logger.Warn(w)
	}
	if err != nil {
		return nil, nil, err
	}

	client := &resticx.Client{
		Runner:   resticx.ExecRunner{},
		CacheDir: cfg.Global.CacheDir,
		Timeout:  cfg.Global.ResticCommandTimeout.Std(),
		Redact:   store.Redactor().Redact,
	}
	return store, client, nil
}

func credentialNames(cfg *config.Config) []string {
	names := make([]string, len(cfg.Credentials))
	for i, c := range cfg.Credentials {
		names[i] = c.Name
	}
	return names
}

func repoNames(cfg *config.Config) []string {
	names := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		names[i] = r.Name
	}
	return names
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate default config: %w", err)
		}
		path = filepath.Join(home, ".config", "resticscope", "config.toml")
	}
	return config.Load(path)
}

// newLogger returns a JSONL file logger at cfg.Global.LogFile, falling back to a
// no-op logger if the file cannot be opened. The TUI must never log to stderr
// (it would corrupt the screen), so file logging is the default everywhere.
func newLogger(cfg *config.Config) *slog.Logger {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg.Global.LogFile == "" {
		return discard
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Global.LogFile), 0o700); err != nil {
		return discard
	}
	f, err := os.OpenFile(cfg.Global.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return discard
	}
	return slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
