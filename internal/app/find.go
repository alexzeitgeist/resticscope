package app

import (
	"context"
	"errors"
	"fmt"

	"resticscope/internal/cache"
	"resticscope/internal/model"
)

// find.go is the headless orchestration behind the "show me other versions of
// this file" view. One restic call per open: `restic find --json --long
// [--host H] <path>`. The host filter narrows by the originating snapshot's
// hostname by default because absolute paths collide across machines; the TUI
// offers a one-key toggle to widen to all hosts. No per-snapshot ls/dump call
// is made; metadata is joined in from the cached snapshot list.

// ErrFindUnknownHost is returned when the originating snapshot's hostname
// cannot be resolved from the cached snapshot list AND the caller did not
// explicitly opt into all-hosts. Returning this rather than silently
// widening preserves the host-narrowing safety property: a default-narrow
// query never decays into a default-broad one (which would surface
// absolute-path collisions across machines as fake "versions" — exactly the
// failure mode the host filter exists to prevent).
var ErrFindUnknownHost = errors.New("find: originating snapshot host unknown")

// FindFileVersionsResult bundles the rows with the host the app actually
// filtered by, so the renderer has one authoritative source for "what filter
// was used" — never two parallel computations that could drift. Host is ""
// exactly when AllHosts is true.
type FindFileVersionsResult struct {
	Host     string // hostname applied as --host, or "" when AllHosts
	AllHosts bool   // mirrors the request flag so the renderer can label "all hosts" without inference
	Rows     []model.FileVersion
}

// FindFileVersions runs the find call and groups the result into distinct
// (size,mtime) versions of the file. Exactly one restic invocation per call:
// no per-snapshot ls and no fresh snapshot fetch (the snapshot map is taken
// from the cached state).
func (a *App) FindFileVersions(ctx context.Context, repoName, snapshotID, p string, allHosts bool) (FindFileVersionsResult, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return FindFileVersionsResult{}, fmt.Errorf("unknown repo %q", repoName)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return FindFileVersionsResult{}, err
	}

	host := ""
	if !allHosts {
		host = a.hostnameOf(repoName, snapshotID)
		if host == "" {
			return FindFileVersionsResult{}, ErrFindUnknownHost
		}
	}

	results, err := a.Restic.FindMatches(ctx, targetOf(r), resticCreds(material), host, p)
	if err != nil {
		return FindFileVersionsResult{}, err
	}

	return FindFileVersionsResult{
		Host:     host,
		AllHosts: allHosts,
		Rows:     model.GroupFileVersions(results, p, a.snapshotsByID(repoName)),
	}, nil
}

// snapshotsByID returns a map of full snapshot id to the cached snapshot
// record for the given repo. A missing or corrupt cache yields an empty map,
// which collaborates with model.GroupFileVersions' tolerant "unknown id →
// occurrence with empty metadata" behavior — the version view still renders,
// just without short ids/times for the unknown ids.
func (a *App) snapshotsByID(repoName string) map[string]model.Snapshot {
	state, err := a.Cache.Load(context.Background(), repoName)
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

// hostnameOf returns the snapshot's Hostname from the cached state or "" on a
// miss. It is kept composable (returns "", not an error) so future callers
// can choose their own miss policy; FindFileVersions is the one that
// promotes a miss to ErrFindUnknownHost.
func (a *App) hostnameOf(repoName, snapshotID string) string {
	state, err := a.Cache.Load(context.Background(), repoName)
	if err != nil {
		// A cache miss/corrupt makes the lookup impossible; treat as unknown so
		// the caller can opt into all-hosts rather than silently widening.
		if errors.Is(err, cache.ErrMiss) || errors.Is(err, cache.ErrCorrupt) {
			return ""
		}
		return ""
	}
	for i := range state.Snapshots {
		if state.Snapshots[i].ID == snapshotID {
			return state.Snapshots[i].Hostname
		}
	}
	return ""
}
