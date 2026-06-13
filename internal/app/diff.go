package app

import (
	"context"

	"resticscope/internal/model"
)

// diff.go is the headless orchestration behind the snapshot-diff view. One
// streamed restic call per open: `restic --no-lock diff --json <first> <second>`.
// The initial TUI open uses chronological order, and the diff view's swap key
// can intentionally reverse it. No cache writes, no secrets in flight: filenames
// live only in the active TUI session and are cleared on leaving the view.

// SnapshotDiff resolves the repo credentials and streams the diff between two
// snapshots. onEntry receives each decoded change as restic emits it; onProgress
// is a coalesced count tick. The terminal SnapshotDiff returned by resticx
// carries the ParseErrors count (entries themselves are delivered through
// onEntry, not in the returned slice).
func (a *App) SnapshotDiff(ctx context.Context, repoName, olderID, newerID string, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return model.SnapshotDiff{}, unknownRepoError(repoName)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return model.SnapshotDiff{}, err
	}
	return a.Restic.StreamDiff(ctx, targetOf(r), resticCreds(material),
		olderID, newerID, a.Cfg.Diff.Timeout.Std(), onEntry, onProgress)
}
