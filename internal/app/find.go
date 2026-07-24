package app

import (
	"context"
	"errors"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// File-version lookup uses one restic find call, narrowed to the originating
// host by default to avoid cross-machine path collisions. Snapshot metadata is
// joined from cache without per-snapshot restic calls.

// ErrFindUnknownHost prevents a host-scoped query from silently widening when
// the originating host is unknown.
var ErrFindUnknownHost = errors.New("find: originating snapshot host unknown")

// FindFileVersionsResult contains grouped versions and the applied host scope.
// Host is empty exactly when AllHosts is true.
type FindFileVersionsResult struct {
	Host     string
	AllHosts bool
	Rows     []model.FileVersion
}

// FindFileVersions groups one restic find result by size and modification time.
// originHost comes from the caller's live selected snapshot rather than possibly
// stale cache; allHosts explicitly disables host narrowing.
func (a *App) FindFileVersions(ctx context.Context, repoName, originHost, p string, allHosts bool) (FindFileVersionsResult, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return FindFileVersionsResult{}, unknownRepoError(repoName)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return FindFileVersionsResult{}, err
	}

	host := ""
	if !allHosts {
		if originHost == "" {
			return FindFileVersionsResult{}, ErrFindUnknownHost
		}
		host = originHost
	}

	results, err := a.Restic.FindMatches(ctx, targetOf(r), resticCreds(material), host, p)
	if err != nil {
		return FindFileVersionsResult{}, err
	}

	return FindFileVersionsResult{
		Host:     host,
		AllHosts: allHosts,
		Rows:     model.GroupFileVersions(results, p, a.snapshotsByID(ctx, repoName)),
	}, nil
}

// snapshotsByID maps cached full IDs to metadata. Missing cache returns nil;
// GroupFileVersions then retains occurrences with empty metadata so the view
// still renders.
func (a *App) snapshotsByID(ctx context.Context, repoName string) map[string]model.Snapshot {
	state, err := a.Cache.Load(ctx, repoName)
	if err != nil {
		return nil
	}
	if len(state.Snapshots) == 0 {
		return nil
	}
	out := make(map[string]model.Snapshot, len(state.Snapshots))
	for _, s := range state.Snapshots {
		out[s.ID] = s
	}
	return out
}
