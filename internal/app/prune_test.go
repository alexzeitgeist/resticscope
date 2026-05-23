package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"resticscope/internal/config"
	"resticscope/internal/resticx"
)

// pruneApp builds an App whose config declares the named repos and points the
// cache dir at root. PruneCache needs only Cfg (and an optional logger).
func pruneApp(root string, repoNames ...string) *App {
	repos := make([]config.Repo, len(repoNames))
	for i, n := range repoNames {
		repos[i] = config.Repo{Name: n}
	}
	return &App{Cfg: &config.Config{
		Global: config.Global{CacheDir: root},
		Repos:  repos,
	}}
}

// seedCache writes a restic-cache subdirectory for name containing a single file
// of the given size, and returns the directory path. The name is sanitized the
// same way restic's RESTIC_CACHE_DIR is, so it matches what PruneCache scans.
func seedCache(t *testing.T, cacheDir, name string, size int) string {
	t.Helper()
	dir := filepath.Join(resticx.CacheRoot(cacheDir), resticx.RepoCacheName(name))
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "pack"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func entryByName(res PruneResult, name string) (CacheEntry, bool) {
	for _, e := range res.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return CacheEntry{}, false
}

// By default prune removes only orphaned caches — those with no configured repo
// — and leaves the caches backing live repos in place.
func TestPruneCacheOrphansOnly(t *testing.T) {
	cacheDir := t.TempDir()
	keepDir := seedCache(t, cacheDir, "live-repo", 100)
	orphanDir := seedCache(t, cacheDir, "removed-repo", 250)

	a := pruneApp(cacheDir, "live-repo")
	res, err := a.PruneCache(context.Background(), false, false)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}

	if res.Freed != 250 {
		t.Errorf("freed = %d, want 250", res.Freed)
	}
	if res.Pruned() != 1 {
		t.Errorf("pruned count = %d, want 1", res.Pruned())
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan cache should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Errorf("live repo cache should be kept, stat err = %v", err)
	}

	live, ok := entryByName(res, "live-repo")
	if !ok || live.Orphan || live.Pruned {
		t.Errorf("live entry = %+v, want kept non-orphan", live)
	}
	orphan, ok := entryByName(res, "removed-repo")
	if !ok || !orphan.Orphan || !orphan.Pruned {
		t.Errorf("orphan entry = %+v, want pruned orphan", orphan)
	}
}

// --all removes every cache, including those backing live repos.
func TestPruneCacheAll(t *testing.T) {
	cacheDir := t.TempDir()
	liveDir := seedCache(t, cacheDir, "live-repo", 100)
	orphanDir := seedCache(t, cacheDir, "removed-repo", 250)

	a := pruneApp(cacheDir, "live-repo")
	res, err := a.PruneCache(context.Background(), true, false)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if res.Freed != 350 || res.Pruned() != 2 {
		t.Errorf("freed=%d pruned=%d, want 350 and 2", res.Freed, res.Pruned())
	}
	for _, d := range []string{liveDir, orphanDir} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("--all should remove %s, stat err = %v", d, err)
		}
	}
}

// A dry run reports what would be freed but deletes nothing.
func TestPruneCacheDryRun(t *testing.T) {
	cacheDir := t.TempDir()
	orphanDir := seedCache(t, cacheDir, "removed-repo", 250)
	seedCache(t, cacheDir, "live-repo", 100)

	a := pruneApp(cacheDir, "live-repo")
	res, err := a.PruneCache(context.Background(), false, true)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if res.Freed != 250 || res.Pruned() != 1 {
		t.Errorf("freed=%d pruned=%d, want 250 and 1", res.Freed, res.Pruned())
	}
	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("dry run must not delete %s, stat err = %v", orphanDir, err)
	}
}

// Orphan detection uses the same sanitization as RESTIC_CACHE_DIR, so a repo
// whose name needs sanitizing is still matched to its on-disk cache and kept.
func TestPruneCacheMatchesSanitizedNames(t *testing.T) {
	cacheDir := t.TempDir()
	dir := seedCache(t, cacheDir, "home/server:1", 100) // dir is "home_server_1"

	a := pruneApp(cacheDir, "home/server:1")
	res, err := a.PruneCache(context.Background(), false, false)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if res.Pruned() != 0 {
		t.Errorf("sanitized name should match its cache; pruned=%d, want 0", res.Pruned())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("matched cache should be kept, stat err = %v", err)
	}
}

// A stray file directly under the restic-cache root is left untouched: prune
// only ever removes per-repo subdirectories it expects restic to have created.
func TestPruneCacheIgnoresStrayFiles(t *testing.T) {
	cacheDir := t.TempDir()
	root := resticx.CacheRoot(cacheDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(root, "README")
	if err := os.WriteFile(stray, []byte("not a cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pruneApp(cacheDir) // no repos -> everything would be an orphan
	res, err := a.PruneCache(context.Background(), true, false)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if len(res.Entries) != 0 || res.Freed != 0 {
		t.Errorf("stray file should be ignored, got %+v", res)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file must not be removed, stat err = %v", err)
	}
}

// A missing restic-cache directory (never refreshed) is not an error.
func TestPruneCacheMissingDir(t *testing.T) {
	a := pruneApp(t.TempDir(), "repo-a")
	res, err := a.PruneCache(context.Background(), false, false)
	if err != nil {
		t.Fatalf("PruneCache over missing dir: %v", err)
	}
	if len(res.Entries) != 0 || res.Freed != 0 {
		t.Errorf("missing dir should yield empty result, got %+v", res)
	}
}

// With no cache_dir configured there is nothing to scan.
func TestPruneCacheNoCacheDir(t *testing.T) {
	a := pruneApp("")
	res, err := a.PruneCache(context.Background(), true, false)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if res.Root != "" || len(res.Entries) != 0 {
		t.Errorf("empty cache dir should yield empty result, got %+v", res)
	}
}
