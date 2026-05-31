package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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

	// Browse index settings. IndexTimeout is generous because a one-time full
	// index of a huge snapshot can take minutes; MaxDiskBytes defaults to a
	// finite session-wide safety cap. Users can still opt into unlimited browse
	// DB growth with max_disk_bytes = "0".
	defaultBrowseIndexTimeout = 10 * time.Minute
	defaultBrowseMaxDiskBytes = 2 << 30
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

	// Browse index settings are seeded with their defaults in Decode (not here)
	// so an explicit index_timeout `0` is distinguishable from an omitted key and
	// reaches validation.

	g.CacheDir = expandPath(g.CacheDir, home)
	if g.LogFile == "" {
		g.LogFile = filepath.Join(g.CacheDir, "log.jsonl")
	} else {
		g.LogFile = expandPath(g.LogFile, home)
	}

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
