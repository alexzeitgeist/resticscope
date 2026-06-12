package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"resticscope/internal/theme"
)

// Defaults applied when a setting is left at its zero value.
const (
	defaultParallelism           = 4
	defaultCacheDir              = "~/.cache/resticscope"
	defaultStaleAfter            = 10 * time.Minute
	defaultStaleGrace            = 12 * time.Hour
	defaultLockMaxAge            = 30 * time.Minute
	defaultSecretsCommandTimeout = 30 * time.Second
	defaultResticCommandTimeout  = 2 * time.Minute
	defaultShellPasswordMode     = "file"

	// Long-running interactive streams. Browse indexing and snapshot diffing
	// compare or walk large trees, so their defaults are intentionally more
	// generous than the generic restic command timeout.
	defaultBrowseIndexTimeout = 10 * time.Minute
	defaultBrowseMaxDiskBytes = 2 << 30
	defaultDiffTimeout        = 10 * time.Minute

	// Extract copies data out of a backup with a read-only restic invocation. A
	// large tree can take minutes, so its timeout is likewise generous, and its
	// output lands under target_root by default.
	defaultExtractTimeout    = 30 * time.Minute
	defaultExtractTargetRoot = "~/resticscope-extracts"
	// Restored symlinks that point outside the tree are kept verbatim (+ warn) by
	// default, matching restic and the wider restore ecosystem.
	defaultUnsafeSymlinks = UnsafeSymlinksKeep
)

// Load reads, normalizes, and validates the config at path. The returned
// Config has defaults applied and ~ expanded against the current user's home.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Decode(data)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "" // expansion becomes a no-op; not fatal
	}
	cfg.Normalize(home)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Decode parses TOML into a Config. Value-normalization defaults live in
// Normalize, not here, with two deliberate exceptions seeded pre-decode:
// refresh_on_open (a plain bool cannot tell an absent key from an explicit
// `false` afterward, so its default-true must precede decoding) and the browse
// index settings. The browse settings are seeded here for the same reason in
// reverse: seeding index_timeout before decode lets an explicit `0` overwrite
// the default and survive into validation as a rejected value, while an omitted
// key keeps the default. max_disk_bytes = "0" remains an accepted explicit
// opt-in to unlimited growth. It rejects unknown keys so typos in config surface
// as errors rather than being silently ignored.
func Decode(data []byte) (*Config, error) {
	cfg := Config{
		Global: Global{RefreshOnOpen: true},
		Browse: Browse{
			IndexTimeout: Duration(defaultBrowseIndexTimeout),
			MaxDiskBytes: defaultBrowseMaxDiskBytes,
		},
		Diff: Diff{Timeout: Duration(defaultDiffTimeout)},
		Extract: Extract{
			TargetRoot:     defaultExtractTargetRoot,
			ExtractTimeout: Duration(defaultExtractTimeout),
			UnsafeSymlinks: defaultUnsafeSymlinks,
		},
		// Like refresh_on_open: a default-true bool must be seeded before the
		// decode so an explicit `background = false` stays distinguishable.
		Theme: Theme{Background: true},
	}
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	return &cfg, nil
}

// Normalize fills in defaults and expands ~ in paths against home. It is split
// from Load so tests can exercise it with an explicit home directory.
func (c *Config) Normalize(home string) {
	g := &c.Global
	if g.Parallelism <= 0 {
		g.Parallelism = defaultParallelism
	}
	if g.CacheDir == "" {
		g.CacheDir = defaultCacheDir
	}
	if g.ShellPasswordMode == "" {
		g.ShellPasswordMode = defaultShellPasswordMode
	}
	if g.StaleAfter == 0 {
		g.StaleAfter = Duration(defaultStaleAfter)
	}
	if g.StaleGrace == 0 {
		g.StaleGrace = Duration(defaultStaleGrace)
	}
	if g.LockMaxAge == 0 {
		g.LockMaxAge = Duration(defaultLockMaxAge)
	}
	if g.SecretsCommandTimeout == 0 {
		g.SecretsCommandTimeout = Duration(defaultSecretsCommandTimeout)
	}
	if g.ResticCommandTimeout == 0 {
		g.ResticCommandTimeout = Duration(defaultResticCommandTimeout)
	}
	// Make the omitted-vs-explicit-empty shape uniform: both become a non-nil
	// empty slice so runtime code only needs to test len.
	if g.GroupBy == nil {
		g.GroupBy = []string{}
	}

	// Browse/diff stream settings are seeded with their defaults in Decode (not
	// here) so an explicit timeout `0` is distinguishable from an omitted key
	// and reaches validation.

	if c.Theme.Name == "" {
		c.Theme.Name = theme.DefaultName
	}

	g.CacheDir = expandPath(g.CacheDir, home)
	if g.LogFile == "" {
		g.LogFile = filepath.Join(g.CacheDir, "log.jsonl")
	} else {
		g.LogFile = expandPath(g.LogFile, home)
	}

	// Extract output base and timeout are both seeded in Decode (not here) so an
	// explicit empty `target_root` / explicit-zero `extract_timeout` is
	// distinguishable from an omitted key: it overwrites the seed, survives to
	// validation, and is rejected there rather than silently defaulted. Here we
	// only expand ~, mirroring CacheDir.
	c.Extract.TargetRoot = expandPath(c.Extract.TargetRoot, home)

	for i := range c.Repos {
		if c.Repos[i].BucketLookup == "" {
			c.Repos[i].BucketLookup = "auto"
		}
	}
}

// expandPath replaces a leading ~ with home. With an empty home it is a no-op.
func expandPath(path, home string) string {
	if home == "" || path == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
