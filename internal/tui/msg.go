package tui

import (
	"resticscope/internal/app"
	"resticscope/internal/model"
)

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

// browseLoadedMsg is delivered when a snapshot browse load (initial or
// load-more) finishes. gen tags the load with the generation that started it, so
// Update can discard a stale result from a cancelled or superseded crawl: a late
// result whose gen no longer matches m.browseGen is dropped, never resurrecting
// abandoned browse state. result is the built session-only tree on success; err
// is an already-redacted restic/secrets failure carrying no path data. Neither is
// ever persisted.
type browseLoadedMsg struct {
	gen    int
	result *model.BrowseResult
	err    error
}

// shellExitedMsg is delivered after an interactive shell-out returns (via
// tea.ExecProcess) or after the session could not be prepared at all. err is the
// shell's exit error or the preparation failure; it never carries a secret
// (app.ShellSession resolves secrets without embedding their values).
type shellExitedMsg struct {
	err error
}
