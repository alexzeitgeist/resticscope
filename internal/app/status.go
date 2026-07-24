package app

import (
	"context"
	"errors"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/cache"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// RepoStatus contains one repository's evaluated status and source state.
type RepoStatus struct {
	Name   string
	Status model.Status
	State  model.RepoState
	Stale  bool // the cache entry is older than global.stale_after
}

// Statuses reads cached status for every repository without backend calls. A
// missing or corrupt entry becomes grey rather than failing, keeping the operation
// side-effect-free.
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
			default:
				return nil, err // genuine error (e.g. context cancelled)
			}
			state = model.RepoState{Name: r.Name}
		}
		rows = append(rows, a.statusRow(now, r, state, staleAfter))
	}
	return rows, nil
}

// RowsFromStates evaluates config-ordered live states without reading cache.
// Status refresh uses it so persistence failures cannot substitute stale data.
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

// WorstExitCode returns zero for green, one for any amber, and two for any red,
// error, or grey status.
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
