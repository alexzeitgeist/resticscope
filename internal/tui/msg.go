package tui

import (
	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Async result messages carry a generation so Update can reject canceled or
// superseded work. Errors arrive redacted; any restic path is shown only in the
// status line and is never cached, logged, or stored in RepoState. Filename and
// path payloads are cleared from model state when their view closes.

// browseIndexProgressMsg carries a monotonic, non-persistent node count from an
// in-flight snapshot index.
type browseIndexProgressMsg struct {
	gen int
	n   int
}

// browseIndexedMsg reports that snapshot indexing finished. On success, the
// committed index is ready for its first directory listing.
type browseIndexedMsg struct {
	gen int
	err error
}

// browseDirMsg carries a session DB directory listing. selectPath identifies
// the child to select, or is empty to select the first row.
type browseDirMsg struct {
	gen        int
	dir        string
	selectPath string
	rows       []model.BrowseEntry
	err        error
}

// browseSearchMsg carries a filename-search result and its exact query so stale
// results can be rejected after further edits.
type browseSearchMsg struct {
	gen    int
	query  string
	result model.BrowseSearchResult
	err    error
}

// findVersionsMsg carries version rows and the host filter actually applied,
// keeping the renderer from recomputing it.
type findVersionsMsg struct {
	gen    int
	result app.FindFileVersionsResult
	err    error
}

// snapshotDiffProgressMsg carries the running number of entries received from
// an in-flight restic diff stream.
type snapshotDiffProgressMsg struct {
	gen  int
	seen int
}

// snapshotDiffMsg carries a completed streamed diff. Update builds its tree on
// the UI thread from entries, while result contains the terminal summary.
type snapshotDiffMsg struct {
	gen      int
	older    model.Snapshot
	newer    model.Snapshot
	metadata bool
	result   model.SnapshotDiff
	entries  []model.DiffEntry
	err      error
}

// diffInfoMsg carries both snapshots' records for the open diff info screen.
type diffInfoMsg struct {
	gen           int
	first, second app.DiffNodeSide
}

// repoRefreshedMsg carries a completed repository refresh. A cache-save error
// does not invalidate its live row; the UI displays both the row and a warning.
type repoRefreshedMsg struct {
	name string
	row  app.RepoStatus
	err  error
}

// shellExitedMsg reports shell preparation, exit, and temporary-password-file
// cleanup failures. Neither error contains a secret; cleanup failure takes
// priority because a lingering password file requires action.
type shellExitedMsg struct {
	err        error
	cleanupErr error
}
