// Package config loads and validates resticscope's TOML configuration.
//
// It never holds secrets: credentials and restic passwords come from the
// secrets_command (see internal/secrets), not from this file. Config only
// describes which repos exist and what is expected of them.
package config

import "time"

// Config is the fully parsed, normalized, validated configuration.
type Config struct {
	Global      Global       `toml:"global"`
	Credentials []Credential `toml:"credentials"`
	Repos       []Repo       `toml:"repos"`
}

// Global holds process-wide settings.
type Global struct {
	Parallelism       int    `toml:"parallelism"`
	CacheDir          string `toml:"cache_dir"`
	LogFile           string `toml:"log_file"`
	RefreshOnOpen     bool   `toml:"refresh_on_open"`
	Shell             string `toml:"shell"`
	ShellPasswordMode string `toml:"shell_password_mode"`
	SecretsCommand    string `toml:"secrets_command"`

	// Durations are TOML strings like "10m", "24h".
	StaleAfter            Duration `toml:"stale_after"`             // cache entries older than this are refreshed on open
	StaleGrace            Duration `toml:"stale_grace"`             // amber band added on top of a repo's expected_frequency
	LockMaxAge            Duration `toml:"lock_max_age"`            // a lock older than this is treated as red
	SecretsCommandTimeout Duration `toml:"secrets_command_timeout"` // timeout for secrets_command
	ResticCommandTimeout  Duration `toml:"restic_command_timeout"`  // timeout for each restic invocation
}

// Credential is an S3 access-key/secret-key pair's coordinates. On Hetzner a
// key pair is project-bound and reaches every bucket in that project, so the
// endpoint/region/bucket_lookup live here, not per repo. The actual keys are
// resolved at runtime from the secrets_command, keyed by Name.
type Credential struct {
	Name         string `toml:"name"`
	Endpoint     string `toml:"endpoint"`
	Region       string `toml:"region"`
	BucketLookup string `toml:"bucket_lookup"` // auto | dns | path
}

// Repo is a single restic repository: a bucket (and optional path) reached via
// a named Credential, plus the coverage expectations used for status.
type Repo struct {
	Name              string            `toml:"name"`
	Description       string            `toml:"description"`
	Credential        string            `toml:"credential"`
	Bucket            string            `toml:"bucket"`
	Path              string            `toml:"path"`
	ExpectedFrequency Duration          `toml:"expected_frequency"`
	ExpectedHosts     []string          `toml:"expected_hosts"`
	ExpectedPaths     []string          `toml:"expected_paths"`
	ExpectedTags      []string          `toml:"expected_tags"`
	Labels            map[string]string `toml:"labels"`
}

// Credential returns the named credential block, if present.
func (c *Config) Credential(name string) (Credential, bool) {
	for _, cr := range c.Credentials {
		if cr.Name == name {
			return cr, true
		}
	}
	return Credential{}, false
}

// Duration is a time.Duration that unmarshals from a TOML string ("24h").
type Duration time.Duration

// UnmarshalText parses a Go duration string, satisfying encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
