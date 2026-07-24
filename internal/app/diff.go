package app

import (
	"context"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Snapshot diffs use one streamed restic call in the requested order without
// writing application cache state. Resticx keeps the password out of argv, and
// the TUI clears streamed filenames when it leaves the diff context.

// SnapshotDiff streams changes between two snapshots to onEntry. onProgress
// receives coalesced count updates; the returned summary contains parse errors,
// not the streamed entries.
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
