package app

import (
	"context"
	"errors"
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

// RefreshRow refreshes one repo (persisting the result) and returns its
// evaluated status row, ready for the TUI to render. The returned error reports
// only a cache-persistence failure; a restic/secrets failure is captured in the
// row's State and Status, never returned as an error. The row is returned even
// when the save fails, so the UI can show live data alongside the warning — the
// same "refresh means live state" contract RefreshAll honors.
func (a *App) RefreshRow(ctx context.Context, name string) (RepoStatus, error) {
	r, ok := a.repo(name)
	if !ok {
		return RepoStatus{}, fmt.Errorf("no repo %q in config", name)
	}
	state, err := a.Refresh(ctx, name)
	row := a.statusRow(a.Clock.Now(), r, state, a.Cfg.Global.StaleAfter.Std())
	return row, err
}

// RefreshAll refreshes every configured repo concurrently, bounded by
// global.parallelism, and persists each result. Results are returned in config
// order and are always the live refresh outcome. Per-repo restic/secrets
// failures are captured in their states, not returned as an error. The returned
// error is non-nil if the context is cancelled or any result could not be
// persisted — a save failure must be surfaced, never swallowed, so callers do
// not mistake stale cache for a fresh refresh.
func (a *App) RefreshAll(ctx context.Context) ([]model.RepoState, error) {
	results := make([]model.RepoState, len(a.Cfg.Repos))
	sem := make(chan struct{}, a.parallelism())
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		saveErrs []error
	)

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
				mu.Lock()
				saveErrs = append(saveErrs, fmt.Errorf("persist %q: %w", r.Name, err))
				mu.Unlock()
			}
			results[i] = state
		}(i, a.Cfg.Repos[i])
	}
	wg.Wait()

	return results, errors.Join(append([]error{ctx.Err()}, saveErrs...)...)
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
		// Phase 0: any restic failure (including a lock) is recorded as an
		// error. The lock-age branch in EvaluateStatus stays unit-tested but
		// dormant here; wiring it needs LockedSince carry-forward from the
		// prior cached state, which belongs with a deliberate history-
		// preserving refresh rather than this path.
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
