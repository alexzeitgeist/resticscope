package app

import (
	"context"
	"sync"

	"github.com/alexzeitgeist/resticscope/internal/config"
)

// RepoCheck is a repository reachability verdict. Err is nil on success;
// otherwise it contains a printable, secret-free resolution or restic error.
type RepoCheck struct {
	Name string
	Err  error
}

// OK reports whether the repo was reachable.
func (r RepoCheck) OK() bool { return r.Err == nil }

// Check probes every configured repository with bounded concurrency and returns
// results in config order. Repository failures populate RepoCheck.Err; the
// top-level error reports only context cancellation. Check does not inspect the
// restic version or update the cache.
func (a *App) Check(ctx context.Context) ([]RepoCheck, error) {
	results := make([]RepoCheck, len(a.Cfg.Repos))
	sem := make(chan struct{}, a.parallelism())
	var wg sync.WaitGroup

	for i, r := range a.Cfg.Repos {
		wg.Go(func() {
			// Respect the parallelism limit, but never block past cancellation.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = RepoCheck{Name: r.Name, Err: ctx.Err()}
				return
			}

			results[i] = RepoCheck{Name: r.Name, Err: a.checkOne(ctx, r)}
		})
	}
	wg.Wait()

	return results, ctx.Err()
}

// checkOne resolves repository secrets and returns only errors safe to display.
func (a *App) checkOne(ctx context.Context, r config.Repo) error {
	// Credential is optional for local and SFTP backends; when set, it must resolve.
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return err
	}
	return a.Restic.CatConfig(ctx, targetOf(r), resticCreds(material))
}
