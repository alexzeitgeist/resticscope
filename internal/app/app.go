// Package app is resticscope's headless orchestration layer. It loads cached
// state, refreshes repositories by driving restic, evaluates status, and writes
// the cache — with no terminal dependency. `status`, `check`, `exec`, and the
// TUI are all thin callers of this package.
//
// Following the engineering rules, app owns no hidden globals: its clock,
// restic runner, cache store, and secrets resolver are all injected as the
// small consumer-side interfaces declared here.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
)

// Clock supplies the current time. Injected so status evaluation is
// deterministic in tests.
type Clock interface {
	Now() time.Time
}

// CacheStore reads and writes per-repo state. Satisfied by *cache.Store.
type CacheStore interface {
	Load(ctx context.Context, name string) (model.RepoState, error)
	Save(ctx context.Context, name string, state model.RepoState) error
}

// Restic runs the restic operations app needs. Satisfied by *resticx.Client.
type Restic interface {
	Snapshots(ctx context.Context, t resticx.Target, creds resticx.Creds) ([]model.Snapshot, error)
	CatConfig(ctx context.Context, t resticx.Target, creds resticx.Creds) error
	StreamSnapshotTree(ctx context.Context, t resticx.Target, creds resticx.Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error)
	FindMatches(ctx context.Context, t resticx.Target, creds resticx.Creds, host, pattern string) ([]model.FindSnapshotResult, error)
	StreamDiff(ctx context.Context, t resticx.Target, creds resticx.Creds, olderID, newerID string, timeout time.Duration, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error)
	ExtractTree(ctx context.Context, t resticx.Target, creds resticx.Creds, params resticx.ExtractTreeParams, onEvent func(resticx.ExtractTreeEvent) error) error
}

// Secrets resolves a repo's runtime credentials. Satisfied by *secrets.Store.
type Secrets interface {
	Resolve(repoName, credName string) (secrets.Material, error)
}

// App wires the dependencies together. Construct it directly; all fields are
// required except Log, Browse, and Priv.
//
// Browse is the lazily-opened, session-scoped encrypted store backing the in-app
// file browser. It is nil for non-TUI entry points (status/check/exec), and the
// browse methods guard that nil.
//
// Priv launches the privileged (sudo) extract helper. It is nil for non-TUI
// entry points and when the running binary cannot be resolved; the extract
// methods guard that nil by refusing privileged requests.
type App struct {
	Cfg     *config.Config
	Secrets Secrets
	Restic  Restic
	Cache   CacheStore
	Clock   Clock
	Log     *slog.Logger
	Browse  *BrowseSession
	Priv    PrivilegedRunner
}

func (a *App) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (a *App) parallelism() int {
	if a.Cfg.Global.Parallelism > 0 {
		return a.Cfg.Global.Parallelism
	}
	return 1
}

func (a *App) repo(name string) (config.Repo, bool) {
	for _, r := range a.Cfg.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}

// ErrUnknownRepo is the sentinel every app entry point returns when asked to act
// on a repo name that is not in the loaded config. It is wrapped (with the name)
// by unknownRepoError so callers can match the condition with errors.Is rather
// than string-comparing the message. The name is the only dynamic part; the
// repo name is not a secret (it is the user's own config label, not a URL or
// credential).
var ErrUnknownRepo = errors.New("unknown repo")

// unknownRepoError builds the "unknown repo" error for a missing a.repo lookup,
// wrapping ErrUnknownRepo so errors.Is(err, ErrUnknownRepo) holds.
func unknownRepoError(name string) error {
	return fmt.Errorf("%w %q", ErrUnknownRepo, name)
}

func (a *App) statusParams(r config.Repo) model.StatusParams {
	return model.StatusParams{
		ExpectedFrequency: r.ExpectedFrequency.Std(),
		StaleGrace:        a.Cfg.Global.StaleGrace.Std(),
		LockMaxAge:        a.Cfg.Global.LockMaxAge.Std(),
	}
}

func targetOf(r config.Repo) resticx.Target {
	return resticx.Target{
		Name:    r.Name,
		Repo:    r.RepositoryURL(),
		Options: r.BackendOptions(),
		Env:     r.BackendEnv(),
	}
}

func resticCreds(m secrets.Material) resticx.Creds {
	return resticx.Creds{
		Env:            m.Env,
		ResticPassword: m.ResticPassword,
	}
}
