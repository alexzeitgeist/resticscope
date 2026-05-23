package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

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
		return refreshExitCode(rows, err)
	}

	rows, err := a.Statuses(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	formatStatusTable(stdout, rows, a.Clock.Now())
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
