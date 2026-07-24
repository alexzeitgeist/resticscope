package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/theme"

	"github.com/BurntSushi/toml"
)

// Default values apply when settings retain their zero values.
const (
	defaultParallelism           = 4
	defaultCacheDir              = "~/.cache/resticscope"
	defaultStaleAfter            = 10 * time.Minute
	defaultStaleGrace            = 12 * time.Hour
	defaultLockMaxAge            = 30 * time.Minute
	defaultSecretsCommandTimeout = 30 * time.Second
	defaultResticCommandTimeout  = 2 * time.Minute
	defaultShellPasswordMode     = "file"

	// Interactive tree operations get longer timeouts than routine restic commands.
	defaultBrowseIndexTimeout = 10 * time.Minute
	defaultBrowseMaxDiskBytes = 2 << 30
	defaultDiffTimeout        = 10 * time.Minute

	// Extraction allows time for large restores and writes under TargetRoot by default.
	defaultExtractTimeout    = 30 * time.Minute
	defaultExtractTargetRoot = "~/resticscope-extracts"
	// Preserve escaping symlinks with a warning by default, matching restic behavior.
	defaultUnsafeSymlinks = UnsafeSymlinksKeep
)

// Load reads, normalizes, and validates the config at path. The returned
// Config has defaults applied and ~ expanded against the current user's home.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the user-selected config file; reading it is the function's purpose
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

// Decode parses TOML and rejects unknown keys. It seeds default-true booleans
// and positive timeouts before decoding so explicit false or zero values survive
// for validation; Normalize applies other defaults. An explicit zero
// MaxDiskBytes remains unlimited.
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
			RememberTarget: true,
		},
		// Seed Theme so explicit false or empty values survive to validation.
		Theme: Theme{Name: theme.DefaultName, Background: true},
	}
	// Decode repos and profiles into maps, then assemble a file-ordered,
	// profile-resolved slice. Embedded Config preserves struct-tag decoding and
	// seeded defaults.
	doc := struct {
		Config
		Repos    map[string]Repo `toml:"repos"`
		Profiles map[string]Repo `toml:"profiles"`
	}{Config: cfg}
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	repos, err := assembleRepos(doc.Repos, doc.Profiles, md)
	if err != nil {
		return nil, err
	}
	cfg = doc.Config
	cfg.Repos = repos
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
// from Load so tests can exercise it with an explicit home directory. Settings
// whose zero value is meaningful are seeded in Decode instead, so an explicit
// timeout `0` or `name = ""` stays distinguishable from an omitted key and
// reaches validation.
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
	// Normalize omitted GroupBy to a non-nil empty slice for runtime callers.
	if g.GroupBy == nil {
		g.GroupBy = []string{}
	}

	g.CacheDir = expandPath(g.CacheDir, home)
	if g.LogFile == "" {
		g.LogFile = filepath.Join(g.CacheDir, "log.jsonl")
	} else {
		g.LogFile = expandPath(g.LogFile, home)
	}

	// Decode seeds Extract defaults; Normalize only expands its target path.
	c.Extract.TargetRoot = expandPath(c.Extract.TargetRoot, home)

	for i := range c.Repos {
		r := &c.Repos[i]
		// Expand a leading ~ only for bare-path URLs; scheme URLs pass through.
		r.URL = expandPath(r.URL, home)
		// Default bucket lookup only for S3 shorthand so URL forms can reject it.
		if r.URL == "" && r.BucketLookup == "" {
			r.BucketLookup = "auto"
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
