package app

import (
	"context"
	"fmt"
	"sync"

	"resticscope/internal/config"
)

// RepoCheck is the reachability verdict for one configured repo. Err is nil
// when the repo was reached; otherwise it is a secret-resolution failure or a
// classified restic error. Both are already free of secret values, so a
// RepoCheck is always safe to print.
type RepoCheck struct {
	Name string
	Err  error
}

// OK reports whether the repo was reachable.
func (r RepoCheck) OK() bool { return r.Err == nil }

// Check reaches every configured repo concurrently — bounded by
// global.parallelism — by resolving its secrets and running `restic cat
// config`. Results come back in config order, one per repo. A per-repo failure
// is recorded in its RepoCheck.Err, never returned as the top-level error; the
// top-level error is non-nil only when the context is cancelled.
//
// Check makes no assumptions about restic's version (the caller gates that with
// resticx.AtLeastMinVersion) and never touches the cache: it is a liveness
// probe for `resticscope check`, not a refresh.
func (a *App) Check(ctx context.Context) ([]RepoCheck, error) {
	results := make([]RepoCheck, len(a.Cfg.Repos))
	sem := make(chan struct{}, a.parallelism())
	var wg sync.WaitGroup

	for i := range a.Cfg.Repos {
		wg.Add(1)
		go func(i int, r config.Repo) {
			defer wg.Done()

			// Respect the parallelism limit, but never block past cancellation.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = RepoCheck{Name: r.Name, Err: ctx.Err()}
				return
			}

			results[i] = RepoCheck{Name: r.Name, Err: a.checkOne(ctx, r)}
		}(i, a.Cfg.Repos[i])
	}
	wg.Wait()

	return results, ctx.Err()
}

// checkOne resolves a repo's secrets and probes it with `restic cat config`.
// Any returned error already excludes secret values (secrets.Resolve and
// resticx both guarantee this), so it is safe to surface to the user.
func (a *App) checkOne(ctx context.Context, r config.Repo) error {
	if _, ok := a.Cfg.Credential(r.Credential); !ok { // unreachable after config validation, but stay defensive
		return fmt.Errorf("credential %q not found", r.Credential)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return err
	}
	return a.Restic.CatConfig(ctx, targetOf(r), resticCreds(material))
}
