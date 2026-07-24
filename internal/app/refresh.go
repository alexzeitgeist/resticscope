package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Refresh updates and persists one repository. Restic failures are stored in the
// returned state's LastError and error status rather than returned as operation
// errors.
func (a *App) Refresh(ctx context.Context, name string) (model.RepoState, error) {
	r, ok := a.repo(name)
	if !ok {
		return model.RepoState{}, unknownRepoError(name)
	}
	state := a.refreshOne(ctx, r)
	if err := a.Cache.Save(ctx, name, state); err != nil {
		return state, fmt.Errorf("save cache for %q: %w", name, err)
	}
	return state, nil
}

// RefreshRow returns a live evaluated row after persisting one refresh. It returns
// the row even when persistence fails; restic and secret failures remain in its
// state rather than the returned error.
func (a *App) RefreshRow(ctx context.Context, name string) (RepoStatus, error) {
	r, ok := a.repo(name)
	if !ok {
		return RepoStatus{}, unknownRepoError(name)
	}
	state, err := a.Refresh(ctx, name)
	row := a.statusRow(a.Clock.Now(), r, state, a.Cfg.Global.StaleAfter.Std())
	return row, err
}

// RefreshAll concurrently updates and persists every configured repository with
// bounded parallelism, returning live results in config order. Repository
// operation failures remain in their states; cancellation and persistence
// failures are returned.
func (a *App) RefreshAll(ctx context.Context) ([]model.RepoState, error) {
	results := make([]model.RepoState, len(a.Cfg.Repos))
	sem := make(chan struct{}, a.parallelism())
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		saveErrs []error
	)

	for i, r := range a.Cfg.Repos {
		wg.Go(func() {
			// Respect the parallelism limit, but never block past cancellation.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = model.RepoState{Name: r.Name, RefreshedAt: a.Clock.Now(), LastError: ctx.Err().Error(), Status: model.StatusError}
				return
			}

			state := a.refreshOne(ctx, r)
			if err := a.Cache.Save(ctx, r.Name, state); err != nil {
				a.logger().Warn("cache save failed", "repo", r.Name, "err", err)
				mu.Lock()
				saveErrs = append(saveErrs, fmt.Errorf("persist %q: %w", r.Name, err))
				mu.Unlock()
			}
			results[i] = state
		})
	}
	wg.Wait()

	return results, errors.Join(append([]error{ctx.Err()}, saveErrs...)...)
}

// refreshOne records failures in a credential-free state instead of returning them.
// Failure preserves the last successful observation and timestamp while
// LastError produces a live error verdict. A successful refresh replaces that
// observation.
func (a *App) refreshOne(ctx context.Context, r config.Repo) model.RepoState {
	now := a.Clock.Now()
	params := a.statusParams(r)

	// A missing prior state naturally produces a cold failure state.
	prior, _ := a.Cache.Load(ctx, r.Name)

	// Preserve prior observations and the last successful RefreshedAt on failure.
	fail := func(msg string) model.RepoState {
		state := prior
		state.Name = r.Name
		state.LastError = msg
		state.Status = model.EvaluateStatus(now, params, state)
		return state
	}

	// Credential is optional for local and SFTP backends; when set, it must resolve.
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return fail(err.Error())
	}

	target := targetOf(r)
	creds := resticCreds(material)

	snaps, err := a.Restic.Snapshots(ctx, target, creds)
	if err != nil {
		return fail(err.Error())
	}

	// Successful snapshots replace prior state and clear its error.
	state := model.RepoState{Name: r.Name, RefreshedAt: now}
	state.Snapshots = snaps
	state.SnapshotCount = len(snaps)
	state.Hosts, state.Tags = model.Observed(snaps)
	state.LastSnapshot = model.LatestSnapshotTime(snaps)

	state.Status = model.EvaluateStatus(now, params, state)
	return state
}
