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
	"resticscope/internal/browsedb"
	"resticscope/internal/cache"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
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
	refresh := fs.Bool("refresh", false, "refresh from the repositories before printing (slow; hits the backend)")
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
			// RefreshAll still returns live states when persistence fails. Warn, but
			// render those states directly so --refresh never falls back to stale cache.
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

// cmdCache dispatches the `cache` subcommands. Only `prune` exists today; it is
// kept as its own command group so future cache operations have a home.
func cmdCache(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: resticscope cache prune [--config PATH] [--all] [--dry-run]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "prune":
		return cmdCachePrune(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown cache subcommand %q\n", sub)
		fmt.Fprintln(stderr, "usage: resticscope cache prune [--config PATH] [--all] [--dry-run]")
		return 2
	}
}

// cmdCachePrune reclaims disk space from restic's own per-repo caches under
// <cache_dir>/restic-cache/ (plan §7, §11). By default it removes only orphaned
// caches — those left behind by a repo that has been removed from the config, or
// renamed (the cache is keyed on the config name, so a rename looks like a fresh
// repo) — so the caches backing live repos survive. `--all` removes every cache
// (restic rebuilds it on next access), and `--dry-run` reports what would go
// without deleting it.
// It reads no secrets and makes no network or restic calls. Exit codes: 0 on
// success (including nothing to prune), 2 on a setup or filesystem failure.
func cmdCachePrune(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	all := fs.Bool("all", false, "remove every repo's restic cache, not just orphaned ones (orphans are caches for repos removed or renamed in the config)")
	dryRun := fs.Bool("dry-run", false, "report what would be removed without deleting anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	a := &app.App{Cfg: cfg, Log: newLogger(cfg)}
	res, err := a.PruneCache(ctx, *all, *dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "cache prune: %v\n", err)
		return 2
	}
	formatPruneResult(stdout, res, *dryRun)
	return 0
}

// cmdSecrets dispatches the `secrets` subcommands. Only `template` exists today;
// it is kept as its own command group so future secrets helpers have a home.
func cmdSecrets(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: resticscope secrets template [--config PATH]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "template":
		return cmdSecretsTemplate(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown secrets subcommand %q\n", sub)
		fmt.Fprintln(stderr, usage)
		return 2
	}
}

// cmdSecretsTemplate prints a blank secrets JSON skeleton — the credentials and
// repos maps pre-filled with the names from config, every value left empty — for
// the user to fill in and store in their secrets backend (plan §3). It is the
// onboarding scaffold: it only loads config, reads no secrets, and makes no
// network or restic calls, so it works before any secret exists and is the first
// step on a new machine or repo. It pairs with `check`, which verifies the
// filled-in secrets resolve. The JSON goes to stdout so it can be piped (e.g.
// `... > s.json` then `pass insert -m ... < s.json`); the guidance line goes to
// stderr so stdout stays clean. Exit codes: 0 on success, 2 if config cannot be
// loaded (consistent with the other commands).
func cmdSecretsTemplate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: resticscope secrets template [--config PATH]"
	fs := flag.NewFlagSet("secrets template", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config.toml (default ~/.config/resticscope/config.toml)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fmt.Fprintln(stderr, usage)
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	out, err := secrets.Template(templateCreds(cfg), repoNames(cfg))
	if err != nil {
		fmt.Fprintf(stderr, "secrets template: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "%s\n", out)
	fmt.Fprintln(stderr, "fill in the blank values, store the result in your secrets backend, then verify with `resticscope check`")
	return 0
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

	if err := browsedb.CleanStaleSessions(cfg.Global.CacheDir); err != nil {
		logger.Warn("browse stale-session cleanup", "err", err)
	}
	// The browse store is opened lazily on the first browse; a run that never
	// browses creates no session directory. Close tears it down on clean exit.
	a.Browse = app.NewBrowseSession(newBrowseOpen(cfg.Global.CacheDir, cfg.Browse.MaxDiskBytes.Bytes()))
	defer func() {
		if err := a.Browse.Close(); err != nil {
			logger.Warn("browse session close", "err", err)
		}
	}()

	// Privileged (sudo) extract support: re-execs this binary as root via the
	// extract-helper subcommand. Wired only for the TUI; failure to resolve the
	// running binary just leaves the feature unavailable.
	if pr, err := app.NewSudoPrivilegedRunner(); err == nil {
		a.Priv = pr
	} else {
		logger.Warn("privileged extract unavailable", "err", err)
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
	checkLine(stdout, "config", "ok", fmt.Sprintf("%s, %s",
		humanize.Count(len(cfg.Repos), "repo", "repos"),
		humanize.Count(len(cfg.CredentialNames()), "credential", "credentials")))

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
	_ = tw.Flush()

	if failures > 0 {
		fmt.Fprintf(stderr, "\ncheck failed: %d of %s unreachable\n",
			failures, humanize.Count(len(checks), "repository", "repositories"))
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

	warnings, err := store.Validate(cfg.CredentialNames(), repoNames(cfg))
	for _, w := range warnings {
		logger.Warn(w)
	}
	if err != nil {
		return nil, nil, err
	}

	client := &resticx.Client{
		Runner:   resticx.ExecRunner{},
		Stream:   resticx.ExecRunner{}, // streaming path for the in-app browser
		CacheDir: cfg.Global.CacheDir,
		Timeout:  cfg.Global.ResticCommandTimeout.Std(),
		Redact:   store.Redactor().Redact,
	}
	return store, client, nil
}

// templateCreds maps each credential the repos reference to the secrets-template
// shape it should scaffold: the s3 access_key/secret_key shorthand when every
// repo using it is the s3 shorthand form, the generic env map otherwise. The
// names come from the repo references (config.CredentialNames), so each is used
// by at least one repo by construction.
func templateCreds(cfg *config.Config) []secrets.TemplateCred {
	names := cfg.CredentialNames()
	out := make([]secrets.TemplateCred, len(names))
	for i, name := range names {
		s3 := true
		for _, r := range cfg.Repos {
			if r.Credential == name && r.URL != "" {
				s3 = false
			}
		}
		out[i] = secrets.TemplateCred{Name: name, S3: s3}
	}
	return out
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
