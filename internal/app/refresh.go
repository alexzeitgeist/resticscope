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
//
// A failed refresh preserves the last-known-good observation rather than blanking
// the repo: a single transient restic failure (e.g. an intermittent "repository
// does not exist") must not erase the snapshots and observed data we last saw.
// The failing attempt is recorded in LastError — which makes EvaluateStatus
// classify the repo as StatusError regardless of the carried-over data — so the
// health verdict stays live while the detail data stays useful until the next
// successful refresh replaces it.
func (a *App) refreshOne(ctx context.Context, r config.Repo) model.RepoState {
	now := a.Clock.Now()
	params := a.statusParams(r)

	prior, _ := a.Cache.Load(ctx, r.Name)

	// fail builds the state for an unsuccessful refresh. It keeps prior's observed
	// data and its RefreshedAt — the time of the last *successful* observation,
	// which stays zero when there was never one — and records the new failure in
	// LastError. The remaining last-known-good observations (Snapshots,
	// SnapshotCount, LastSnapshot, Hosts, Tags) carry over untouched.
	fail := func(msg string) model.RepoState {
		state := prior
		state.Name = r.Name
		state.LastError = msg
		state.Status = model.EvaluateStatus(now, params, state)
		return state
	}

	if _, ok := a.Cfg.Credential(r.Credential); !ok { // unreachable after config validation, but stay defensive
		return fail(fmt.Sprintf("credential %q not found", r.Credential))
	}

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

	// A successful snapshots call defines a fresh, fully live state: RefreshedAt
	// advances to now and the prior LastError (if any) is gone.
	state := model.RepoState{Name: r.Name, RefreshedAt: now}
	state.Snapshots = snaps
	state.SnapshotCount = len(snaps)
	state.Hosts, state.Tags = model.Observed(snaps)
	state.LastSnapshot = model.LatestSnapshotTime(snaps)

	state.Status = model.EvaluateStatus(now, params, state)
	return state
}
