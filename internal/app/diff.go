package app

import (
	"context"
	"errors"
	"path"
	"sync"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Snapshot diffs use one streamed restic call in the requested order without
// writing application cache state. Resticx keeps the password out of argv, and
// the TUI clears streamed filenames when it leaves the diff context.

// SnapshotDiff streams changes between two snapshots to onEntry. metadata asks
// restic for metadata-only changes as well. onProgress receives coalesced count
// updates; the returned summary contains parse errors, not the streamed entries.
func (a *App) SnapshotDiff(ctx context.Context, repoName, olderID, newerID string, metadata bool, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return model.SnapshotDiff{}, unknownRepoError(repoName)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return model.SnapshotDiff{}, err
	}
	return a.Restic.StreamDiff(ctx, targetOf(r), resticCreds(material),
		olderID, newerID, metadata, a.Cfg.Diff.Timeout.Std(), onEntry, onProgress)
}

// errDiffNodeRoot rejects an info lookup for the diff root, which restic never
// reports as a change.
var errDiffNodeRoot = errors.New("diff info: the root has no entry of its own")

// DiffNodeSide is one snapshot's record of a diff path. Found is false when the
// snapshot has no entry there, and Err reports a lookup that failed.
type DiffNodeSide struct {
	Node  model.TreeNode
	Found bool
	Err   error
}

// DiffNodes reads the record of the diff path p from both snapshots at once,
// skipping a side whose ID is empty. Each lookup is one restic run under the
// diff timeout.
func (a *App) DiffNodes(ctx context.Context, repoName, p, firstID, secondID string) (first, second DiffNodeSide) {
	fail := func(err error) (DiffNodeSide, DiffNodeSide) {
		if firstID != "" {
			first.Err = err
		}
		if secondID != "" {
			second.Err = err
		}
		return first, second
	}
	r, ok := a.repo(repoName)
	if !ok {
		return fail(unknownRepoError(repoName))
	}
	dir := model.DiffParentOf(p)
	if dir == "" {
		return fail(errDiffNodeRoot)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return fail(err)
	}
	t, creds, name, timeout := targetOf(r), resticCreds(material), path.Base(p), a.Cfg.Diff.Timeout.Std()
	lookup := func(id string) DiffNodeSide {
		if id == "" {
			return DiffNodeSide{}
		}
		n, found, err := a.Restic.TreeNode(ctx, t, creds, id, dir, name, timeout)
		return DiffNodeSide{Node: n, Found: found, Err: err}
	}
	var wg sync.WaitGroup
	wg.Go(func() { first = lookup(firstID) })
	second = lookup(secondID)
	wg.Wait()
	return first, second
}
