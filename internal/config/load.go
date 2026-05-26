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

	// Browse caps. A single crawl stops at the first of these it hits; load-more
	// raises the entry/byte caps toward the session ceilings but never the
	// timeout. See config.Browse for the rationale.
	defaultBrowseMaxEntries          = 200_000
	defaultBrowseMaxJSONBytes        = 128 << 20
	defaultBrowseTimeout             = 120 * time.Second
	defaultBrowseMaxSessionEntries   = 1_000_000
	defaultBrowseMaxSessionJSONBytes = 768 << 20
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
// caps. The browse caps are seeded here for the same reason in reverse: seeding
// them before decode lets an explicit `0` overwrite the default and survive into
// validation as a rejected value, while an omitted key keeps the default. It
// rejects unknown keys so typos in config surface as errors rather than being
// silently ignored.
func Decode(data []byte) (*Config, error) {
	cfg := Config{
		Global: Global{RefreshOnOpen: true},
		Browse: Browse{
			MaxEntries:          defaultBrowseMaxEntries,
			MaxJSONBytes:        defaultBrowseMaxJSONBytes,
			Timeout:             Duration(defaultBrowseTimeout),
			MaxSessionEntries:   defaultBrowseMaxSessionEntries,
			MaxSessionJSONBytes: defaultBrowseMaxSessionJSONBytes,
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

	// Browse caps are seeded with their defaults in Decode (not here) so an
	// explicit `0` is distinguishable from an omitted key and reaches validation.

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
