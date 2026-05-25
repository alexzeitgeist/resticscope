package app

import (
	"context"
	"fmt"

	"resticscope/internal/model"
)

// browse.go orchestrates the in-app snapshot file browser. BrowseSnapshot
// resolves a repo's credentials (exactly like refreshOne), streams one recursive
// `restic ls` through resticx bounded by the given limits, and builds the result
// into a session-only tree. It is deliberately read-only and ephemeral: it never
// touches Cache or RepoState, so no filename or path data is ever persisted. The
// caller (the TUI) holds the BrowseResult in memory only and drops it on leave.

// BrowseSnapshot lists one snapshot's namespace into an in-memory tree, bounded
// by limits. Secret and restic failures are returned as already-redacted errors
// and are not written anywhere. Nothing here persists the result.
func (a *App) BrowseSnapshot(ctx context.Context, repoName, snapshotID string, limits model.BrowseLimits) (model.BrowseResult, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return model.BrowseResult{}, fmt.Errorf("unknown repo %q", repoName)
	}
	if _, ok := a.Cfg.Credential(r.Credential); !ok { // unreachable after config validation, but stay defensive
		return model.BrowseResult{}, fmt.Errorf("credential %q not found", r.Credential)
	}

	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return model.BrowseResult{}, err
	}

	scan, err := a.Restic.ListSnapshotTree(ctx, targetOf(r), resticCreds(material), snapshotID, limits)
	if err != nil {
		return model.BrowseResult{}, err
	}
	return model.BuildBrowseTree(scan, limits), nil
}
