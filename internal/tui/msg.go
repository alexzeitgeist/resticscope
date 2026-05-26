package tui

import (
	"resticscope/internal/app"
	"resticscope/internal/model"
)

// browseIndexProgressMsg carries a running node count from an in-flight snapshot
// index. It is re-armed by waitForIndexProgress after each tick, so the count
// climbs live while the index transaction runs. gen tags it with the generation
// that started the index; a tick whose gen no longer matches m.browseGen is from
// a cancelled or superseded index and is dropped. The count is monotonic guidance
// for the user, never persisted.
type browseIndexProgressMsg struct {
	gen int
	n   int
}

// browseIndexedMsg is delivered when a snapshot's one-time index finishes (or
// fails). gen tags it with the generation that started the index so Update can
// discard a stale result from a cancelled or superseded crawl. err is non-nil on
// failure — an already-redacted restic/secrets/store failure carrying no path
// data; on success the index has committed and the first directory can be listed.
// Nothing here is ever persisted to the cache, RepoState, or any log.
type browseIndexedMsg struct {
	gen int
	err error
}

// browseDirMsg carries the result of a directory listing query against the
// session DB. gen guards against stale listings from a superseded navigation.
// dir is the listed directory; selectPath is the child the cursor should land on
// (empty for the top of the list); rows are that directory's children. err is a
// path-free store error. Rows hold filenames only for the lifetime of the model
// state and are cleared on leaving browse.
type browseDirMsg struct {
	gen        int
	dir        string
	selectPath string
	rows       []model.BrowseEntry
	err        error
}

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

// shellExitedMsg is delivered after an interactive shell-out returns (via
// tea.ExecProcess) or after the session could not be prepared at all. err is the
// shell's exit error or the preparation failure; it never carries a secret
// (app.ShellSession resolves secrets without embedding their values).
type shellExitedMsg struct {
	err error
}
