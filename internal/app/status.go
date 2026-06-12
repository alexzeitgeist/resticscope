package app

import (
	"context"
	"errors"
	"time"

	"resticscope/internal/cache"
	"resticscope/internal/config"
	"resticscope/internal/model"
)

// RepoStatus is one repo's evaluated status plus the cached state it was
// derived from. It is what `resticscope status` and the TUI list view render.
type RepoStatus struct {
	Name   string
	Status model.Status
	State  model.RepoState
	Stale  bool // the cache entry is older than global.stale_after
}

// Statuses returns the current status of every configured repo, reading the
// cache only — no backend or restic calls. A missing or corrupt cache entry yields a
// grey (never-refreshed) status rather than an error, so `status` stays fast
// and side-effect-free for cron and scripts.
func (a *App) Statuses(ctx context.Context) ([]RepoStatus, error) {
	now := a.Clock.Now()
	staleAfter := a.Cfg.Global.StaleAfter.Std()

	rows := make([]RepoStatus, 0, len(a.Cfg.Repos))
	for _, r := range a.Cfg.Repos {
		state, err := a.Cache.Load(ctx, r.Name)
		if err != nil {
			switch {
			case errors.Is(err, cache.ErrCorrupt):
				a.logger().Warn("cache corrupt; treating repo as cold", "repo", r.Name)
			case errors.Is(err, cache.ErrMiss):
				// cold; nothing to log
			default:
				return nil, err // genuine error (e.g. context cancelled)
			}
			state = model.RepoState{Name: r.Name}
		}
		rows = append(rows, a.statusRow(now, r, state, staleAfter))
	}
	return rows, nil
}

// RowsFromStates evaluates already-known live states (e.g. the result of
// RefreshAll) into status rows without touching the cache. This is what
// `status --refresh` renders, so a failed cache write can never make a refresh
// display — or exit on — stale data. states must be in config order, one per
// configured repo.
func (a *App) RowsFromStates(states []model.RepoState) []RepoStatus {
	now := a.Clock.Now()
	staleAfter := a.Cfg.Global.StaleAfter.Std()

	rows := make([]RepoStatus, 0, len(states))
	for i, r := range a.Cfg.Repos {
		if i >= len(states) {
			break
		}
		rows = append(rows, a.statusRow(now, r, states[i], staleAfter))
	}
	return rows
}

// statusRow evaluates one repo's state into a render-ready row.
func (a *App) statusRow(now time.Time, r config.Repo, state model.RepoState, staleAfter time.Duration) RepoStatus {
	row := RepoStatus{
		Name:   r.Name,
		Status: model.EvaluateStatus(now, a.statusParams(r), state),
		State:  state,
	}
	if state.Refreshed() && staleAfter > 0 && now.Sub(state.RefreshedAt) > staleAfter {
		row.Stale = true
	}
	return row
}

// WorstExitCode maps a set of statuses to a process exit code: 0 if all green,
// 1 if any amber, 2 if any red/error/grey. It is the contract `resticscope
// status` exposes to cron and shell conditionals (plan §9).
func WorstExitCode(rows []RepoStatus) int {
	code := 0
	for _, r := range rows {
		switch r.Status {
		case model.StatusGreen:
		case model.StatusAmber:
			if code < 1 {
				code = 1
			}
		default: // red, error, grey
			return 2
		}
	}
	return code
}
