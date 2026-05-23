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
	Stats(ctx context.Context, t resticx.Target, creds resticx.Creds) (model.Stats, error)
}

// Secrets resolves a repo's runtime credentials. Satisfied by *secrets.Store.
type Secrets interface {
	Resolve(repoName, credName string) (secrets.Material, error)
}

// App wires the dependencies together. Construct it directly; all fields are
// required except Log.
type App struct {
	Cfg     *config.Config
	Secrets Secrets
	Restic  Restic
	Cache   CacheStore
	Clock   Clock
	Log     *slog.Logger
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

func (a *App) statusParams(r config.Repo) model.StatusParams {
	return model.StatusParams{
		ExpectedFrequency: r.ExpectedFrequency.Std(),
		StaleGrace:        a.Cfg.Global.StaleGrace.Std(),
		LockMaxAge:        a.Cfg.Global.LockMaxAge.Std(),
	}
}

func expectationOf(r config.Repo) model.Expectation {
	return model.Expectation{
		Hosts:     r.ExpectedHosts,
		Paths:     r.ExpectedPaths,
		Tags:      r.ExpectedTags,
		Frequency: r.ExpectedFrequency.Std(),
	}
}

func targetOf(r config.Repo, cred config.Credential) resticx.Target {
	return resticx.Target{
		Name:         r.Name,
		Endpoint:     cred.Endpoint,
		Region:       cred.Region,
		BucketLookup: cred.BucketLookup,
		Bucket:       r.Bucket,
		Path:         r.Path,
	}
}

func resticCreds(m secrets.Material) resticx.Creds {
	return resticx.Creds{
		AccessKey:      m.AccessKey,
		SecretKey:      m.SecretKey,
		ResticPassword: m.ResticPassword,
	}
}
