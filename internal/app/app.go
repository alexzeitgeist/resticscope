// Package app provides the headless workflows used by resticscope's CLI and
// TUI. It drives restic, evaluates repository status, and manages cached state
// through explicitly injected dependencies.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
	"github.com/alexzeitgeist/resticscope/internal/secrets"
)

// Clock supplies the current time for deterministic status evaluation.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// CacheStore reads and writes per-repository state.
type CacheStore interface {
	// Load returns stored state for name.
	Load(ctx context.Context, name string) (model.RepoState, error)
	// Save stores state for name.
	Save(ctx context.Context, name string, state model.RepoState) error
}

// Restic runs the restic operations required by the application.
type Restic interface {
	// Snapshots returns the snapshots for a target.
	Snapshots(ctx context.Context, t resticx.Target, creds resticx.Creds) ([]model.Snapshot, error)
	// CatConfig verifies access by reading the repository config.
	CatConfig(ctx context.Context, t resticx.Target, creds resticx.Creds) error
	// StreamSnapshotTree streams snapshot nodes to onNode and returns a completion summary.
	StreamSnapshotTree(ctx context.Context, t resticx.Target, creds resticx.Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error)
	// FindMatches returns path matches, optionally restricted to host.
	FindMatches(ctx context.Context, t resticx.Target, creds resticx.Creds, host, pattern string) ([]model.FindSnapshotResult, error)
	// StreamDiff streams snapshot changes and progress to their callbacks.
	StreamDiff(ctx context.Context, t resticx.Target, creds resticx.Creds, olderID, newerID string, metadata bool, timeout time.Duration, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error)
	// ExtractTree restores a tree and sends progress and summary events to onEvent.
	ExtractTree(ctx context.Context, t resticx.Target, creds resticx.Creds, params resticx.ExtractTreeParams, onEvent func(resticx.ExtractTreeEvent) error) error
}

// Secrets resolves a repository's runtime credentials.
type Secrets interface {
	// Resolve returns runtime material for a repository credential.
	Resolve(repoName, credName string) (secrets.Material, error)
}

// App coordinates workflows through per-method dependencies. Callers provide
// Cfg for command workflows except privileged probes; Cache and Clock for
// status; Secrets and Restic for refresh and check; Secrets alone for shell;
// Browse for browsing; and Priv for privileged extraction. Log is optional.
// Browse and Priv methods reject nil; other methods may panic on missing deps.
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

// ErrUnknownRepo identifies a repository name absent from the loaded config.
// Wrapped errors include the requested name and support errors.Is.
var ErrUnknownRepo = errors.New("unknown repo")

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
