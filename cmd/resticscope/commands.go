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

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/browsedb"
	"github.com/alexzeitgeist/resticscope/internal/cache"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
	"github.com/alexzeitgeist/resticscope/internal/secrets"
	"github.com/alexzeitgeist/resticscope/internal/tui"
	"github.com/alexzeitgeist/resticscope/internal/version"
)

// cmdVersion prints the resticscope and restic versions, substitutes a
// not-found message when restic is unavailable, and always returns 0.
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

// cmdStatus prints cached or refreshed repository status and returns the worst
// row's exit code. Configuration, setup, and refresh-level failures return at
// least 2.
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

// cmdCache dispatches cache subcommands.
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

// cmdCachePrune removes restic caches under <cache_dir>/restic-cache. By default
// it removes caches absent from the config; --all includes live caches and
// --dry-run deletes nothing. It loads no secrets and calls neither restic nor the
// network. Setup and filesystem failures return 2.
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

// cmdSecrets dispatches secrets subcommands.
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

// cmdSecretsTemplate prints a config-derived blank secrets document to stdout
// and guidance to stderr. It loads no secrets and calls neither restic nor the
// network, so it works before secrets exist. Configuration failures return 2.
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

// cmdTUI resolves secrets before Bubble Tea starts so passphrase prompts use the
// normal terminal. Resolution failures return 2 instead of launching a TUI that
// can show only stale cache.
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

	// Privileged extraction re-execs this binary through sudo; failure to resolve
	// it leaves the TUI feature unavailable.
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

// cmdCheck validates config, secrets, the restic version, and repository
// reachability without using the cache. It returns 0 when all checks pass, 1
// when completed checks find problems, and 2 when checking cannot complete.
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
		// Command failures suppress provider output, and validation errors omit
		// values; malformed JSON errors may still quote one input byte.
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

// checkRestic validates the restic version before probing repositories because
// unsupported versions have unreliable exit-code and JSON behavior. Probe
// errors invalidate partial results and return 2. Injected functions keep the
// version-gating and cancellation paths hermetic.
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
		// Probe errors invalidate partial repository results.
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

// refreshExitCode returns the worst row code, floored at 2 by refresh failures
// so callers cannot mistake unpersisted state for success.
func refreshExitCode(rows []app.RepoStatus, refreshErr error) int {
	code := app.WorstExitCode(rows)
	if refreshErr != nil && code < 2 {
		code = 2
	}
	return code
}

// refreshDeps loads and validates secrets, then constructs a redacting restic
// client so credentials cannot reach logs or errors.
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

// templateCreds selects S3 key fields only for credentials used exclusively by
// shorthand S3 repositories; all others receive environment maps.
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

// newLogger returns a JSONL file logger or a discard logger on setup failure.
// It never logs to stderr, which would corrupt the TUI.
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
