package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

// CacheEntry describes one repository cache directory. Name is its sanitized
// directory name as produced by resticx.RepoCacheName.
type CacheEntry struct {
	Name     string
	Path     string
	Size     int64 // bytes used by this cache directory
	IsOrphan bool  // no configured repo maps to this directory
	IsPruned bool  // selected for removal (removed on disk, unless this was a dry run)
}

// PruneResult summarizes a PruneCache run.
type PruneResult struct {
	Root    string       // the restic-cache directory that was scanned ("" if none)
	Entries []CacheEntry // every per-repo cache found, in directory-name order
	Freed   int64        // bytes pruned (bytes that would be pruned, for a dry run)
}

// Pruned reports how many cache directories were selected for removal.
func (r PruneResult) Pruned() int {
	n := 0
	for _, e := range r.Entries {
		if e.IsPruned {
			n++
		}
	}
	return n
}

// PruneCache removes restic's per-repository caches without touching resticscope
// state JSON. By default it selects only caches without a configured repository;
// all selects every cache, and dryRun reports without deleting. Restic rebuilds
// removed caches on demand. Missing or unset cache storage returns an empty result.
func (a *App) PruneCache(ctx context.Context, all, dryRun bool) (PruneResult, error) {
	root := resticx.CacheRoot(a.Cfg.Global.CacheDir)
	res := PruneResult{Root: root}
	if root == "" {
		return res, nil
	}

	dirEntries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("read restic cache dir: %w", err)
	}

	known := a.knownCacheNames()
	for _, de := range dirEntries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Ignore root files rather than deleting entries we did not create.
		if !de.IsDir() {
			continue
		}
		entry := CacheEntry{
			Name:     de.Name(),
			Path:     filepath.Join(root, de.Name()),
			IsOrphan: !known[de.Name()],
		}
		size, err := dirSize(ctx, entry.Path)
		if err != nil {
			return res, err
		}
		entry.Size = size

		if all || entry.IsOrphan {
			entry.IsPruned = true
			if !dryRun {
				if err := os.RemoveAll(entry.Path); err != nil {
					return res, fmt.Errorf("remove %s: %w", entry.Path, err)
				}
			}
			res.Freed += entry.Size
		}
		res.Entries = append(res.Entries, entry)
	}

	a.logger().Info("pruned restic cache",
		"root", root, "all", all, "dry_run", dryRun,
		"caches", len(res.Entries), "removed", res.Pruned(), "freed_bytes", res.Freed)
	return res, nil
}

// knownCacheNames returns configured repository cache directory names.
func (a *App) knownCacheNames() map[string]bool {
	known := make(map[string]bool, len(a.Cfg.Repos))
	for _, r := range a.Cfg.Repos {
		known[resticx.RepoCacheName(r.Name)] = true
	}
	return known
}

// dirSize sums regular files while honoring cancellation.
func dirSize(ctx context.Context, path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
