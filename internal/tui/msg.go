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
// failure — a restic/secrets/store failure with secrets already redacted. It may
// transiently carry a filesystem path that restic itself printed on stderr (the
// secret redactor does not strip paths; the scoped shell is the path-free
// alternative): that text is only ever shown in the status line, never persisted
// to the cache, RepoState, or any log. On success the index has committed and
// the first directory can be listed.
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

// browseSearchMsg carries the result of one global filename search against the
// session store. gen guards against a superseded keystroke's result, and query
// is the exact query the search ran for, so applyBrowseSearch can drop a result
// the user has since edited past. result holds matched filenames/paths for the
// lifetime of the model state only (cleared on leaving search/browse); err is a
// path-free store error surfaced in the footer while search stays open.
type browseSearchMsg struct {
	gen    int
	query  string
	result model.BrowseSearchResult
	err    error
}

// findVersionsMsg carries the outcome of one find-file-versions call. gen guards
// against a superseded request's late result (e.g. the user pressed `a` to widen
// the filter, then `q` to leave before the original response arrived). result
// bundles the rows with the host the app actually filtered by, so the renderer
// has one authoritative source for "what filter was used" — never two parallel
// computations that could drift. err is non-nil on failure: a find/secrets/cache
// error with secrets already redacted by Client.classify(); a restic-printed
// filesystem path may transiently appear in the status line, never on disk
// (same caveat as browse). The result's Rows hold filenames/snapshot ids only
// for the lifetime of the model state and are cleared on leaving the view.
type findVersionsMsg struct {
	gen    int
	result app.FindFileVersionsResult
	err    error
}

// snapshotDiffProgressMsg carries a running count of entries seen on the wire
// from an in-flight `restic diff --json` stream. It is re-armed by
// waitForDiffProgress after each tick so the loading line climbs live. gen tags
// it with the generation that started the diff; a tick whose gen no longer
// matches m.diffGen is from a superseded or cancelled diff and is dropped.
type snapshotDiffProgressMsg struct {
	gen  int
	seen int
}

// snapshotDiffMsg is delivered when one streamed diff finishes. gen guards
// against a superseded request's late result. result carries the terminal
// SnapshotDiff (ParseErrors count); entries is the slice the streaming
// onEntry callback accumulated during the run, handed back here so the
// terminal Update can BuildDiffTree once on the UI thread. err is non-nil on
// failure: a restic/secrets error with secrets already redacted (a
// restic-printed filesystem path may transiently appear in the status line,
// never on disk — same caveat as browse). The entries hold paths only for
// the lifetime of the model; clearSnapshotDiff zeroes them on leaving the view.
type snapshotDiffMsg struct {
	gen     int
	result  model.SnapshotDiff
	entries []model.DiffEntry
	err     error
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
