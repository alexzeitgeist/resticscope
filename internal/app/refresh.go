package app

import (
	"context"
	"fmt"
	"sync"

	"resticscope/internal/config"
	"resticscope/internal/model"
)

// Refresh refreshes a single repo by name, persists the result, and returns the
// new state. A refresh that fails to reach restic is not an error of Refresh
// itself: the failure is recorded in the returned state's LastError (and its
// status becomes "error") and still cached, so the UI can show it.
func (a *App) Refresh(ctx context.Context, name string) (model.RepoState, error) {
	r, ok := a.repo(name)
	if !ok {
		return model.RepoState{}, fmt.Errorf("no repo %q in config", name)
	}
	state := a.refreshOne(ctx, r)
	if err := a.Cache.Save(ctx, name, state); err != nil {
		return state, fmt.Errorf("save cache for %q: %w", name, err)
	}
	return state, nil
}

// RefreshAll refreshes every configured repo concurrently, bounded by
// global.parallelism, and persists each result. Results are returned in config
// order. Per-repo failures are captured in their states, not returned as an
// error; the returned error is non-nil only if the context is cancelled.
func (a *App) RefreshAll(ctx context.Context) ([]model.RepoState, error) {
	results := make([]model.RepoState, len(a.Cfg.Repos))
	sem := make(chan struct{}, a.parallelism())
	var wg sync.WaitGroup

	for i := range a.Cfg.Repos {
		wg.Add(1)
		go func(i int, r config.Repo) {
			defer wg.Done()

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
			}
			results[i] = state
		}(i, a.Cfg.Repos[i])
	}
	wg.Wait()
	return results, ctx.Err()
}

// refreshOne does the actual work for a repo and returns its new state. It never
// returns an error: any failure is recorded on the state so the caller can
// cache and display it. Error strings stored here come from secrets.Resolve
// (which never embeds secret values) and resticx (which redacts stderr), so the
// cache stays free of credentials.
func (a *App) refreshOne(ctx context.Context, r config.Repo) model.RepoState {
	now := a.Clock.Now()
	state := model.RepoState{Name: r.Name, RefreshedAt: now}
	params := a.statusParams(r)

	cred, ok := a.Cfg.Credential(r.Credential)
	if !ok { // unreachable after config validation, but stay defensive
		state.LastError = fmt.Sprintf("credential %q not found", r.Credential)
		state.Status = model.EvaluateStatus(now, params, state)
		return state
	}

	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		state.LastError = err.Error()
		state.Status = model.EvaluateStatus(now, params, state)
		return state
	}

	target := targetOf(r, cred)
	creds := resticCreds(material)

	snaps, err := a.Restic.Snapshots(ctx, target, creds)
	if err != nil {
		state.LastError = err.Error()
		state.Status = model.EvaluateStatus(now, params, state)
		return state
	}

	state.Snapshots = snaps
	state.SnapshotCount = len(snaps)
	state.Hosts, state.Paths, state.Tags = model.Observed(snaps)
	state.LastSnapshot = model.LatestSnapshotTime(snaps)

	// Stats are best-effort: slow on big repos, and a refresh stays useful
	// without them.
	if stats, sErr := a.Restic.Stats(ctx, target, creds); sErr != nil {
		state.PartialErr = sErr.Error()
	} else {
		state.TotalSize = stats.TotalSize
		state.PackCount = stats.TotalBlobCount
	}

	state.Status = model.EvaluateStatus(now, params, state)
	return state
}
