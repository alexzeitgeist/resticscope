package tui

import "resticscope/internal/app"

// repoRefreshedMsg is delivered when a single repo's background refresh
// finishes. row carries the freshly evaluated status; err is non-nil only when
// the result could not be persisted to the cache — the row is still the live
// result, so the UI shows fresh data and surfaces the save failure as a warning
// rather than reverting to stale cache.
type repoRefreshedMsg struct {
	name string
	row  app.RepoStatus
	err  error
}
