// Package config loads and validates resticscope's TOML configuration.
//
// It never holds secrets: credentials and restic passwords come from the
// secrets_command (see internal/secrets), not from this file. Config only
// describes which repos exist and what is expected of them.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the fully parsed, normalized, validated configuration.
type Config struct {
	Global      Global       `toml:"global"`
	Credentials []Credential `toml:"credentials"`
	Repos       []Repo       `toml:"repos"`
	Browse      Browse       `toml:"browse"`
}

// Browse bounds the in-app snapshot file browser. A browse loads one streamed
// `restic ls --recursive`, capped so a huge snapshot can't exhaust memory or
// hang: MaxEntries/MaxJSONBytes/Timeout bound a single crawl, and the
// MaxSession* ceilings bound how far repeated "load more" can raise the entry
// and byte caps. Timeout is never raised by load-more. None of the browse data
// is ever persisted; these caps only govern the session-only in-memory tree.
type Browse struct {
	MaxEntries          int      `toml:"max_entries"`
	MaxJSONBytes        ByteSize `toml:"max_json_bytes"`
	Timeout             Duration `toml:"timeout"`
	MaxSessionEntries   int      `toml:"max_session_entries"`
	MaxSessionJSONBytes ByteSize `toml:"max_session_json_bytes"`
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

// Credential names an S3 access-key/secret-key pair. On Hetzner a key pair is
// project-bound and reaches every bucket in the project — including buckets in
// different regions — so a credential carries no location: endpoint, region, and
// bucket_lookup live on the repo. The block exists to declare which key pairs
// exist (so a dangling repo reference is caught) and to document what
// secrets_command must provide. The actual keys are resolved at runtime from the
// secrets_command, keyed by Name.
type Credential struct {
	Name string `toml:"name"`
}

// Repo is a single restic repository: a bucket (and optional path) at an
// endpoint, reached with a named Credential's keys, plus the expected_frequency
// that drives its freshness status. Endpoint/region/bucket_lookup live here, not
// on the credential, so one key pair can back buckets in several regions.
type Repo struct {
	Name              string            `toml:"name"`
	Description       string            `toml:"description"`
	Credential        string            `toml:"credential"`
	Endpoint          string            `toml:"endpoint"`
	Region            string            `toml:"region"`
	Bucket            string            `toml:"bucket"`
	Path              string            `toml:"path"`
	BucketLookup      string            `toml:"bucket_lookup"` // auto | dns | path
	ExpectedFrequency Duration          `toml:"expected_frequency"`
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

// ByteSize is a byte count that unmarshals from a TOML string with a binary unit
// suffix ("128MiB", "768MiB"). It mirrors Duration: a named integer with a text
// unmarshaler and a typed accessor, so the browse caps read naturally in config.
type ByteSize int64

// UnmarshalText parses a byte-size string, satisfying encoding.TextUnmarshaler.
// It accepts KiB/MiB/GiB (and the KB/MB/GB and bare K/M/G/B variants) and
// rejects negative or unparseable values.
func (b *ByteSize) UnmarshalText(text []byte) error {
	n, err := parseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = ByteSize(n)
	return nil
}

// Bytes returns the size as a plain int64.
func (b ByteSize) Bytes() int64 { return int64(b) }

// parseByteSize parses a human byte size like "128MiB". It is intentionally a
// duplicate of the parser in tools/restic-ls-poc: that is a dev measurement
// tool, and product code must not import dev tooling. It accepts a bare number
// (bytes) and the common binary/decimal-ish suffixes, treating e.g. KB and KiB
// alike (1024); it rejects negative values.
func parseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, errors.New("empty byte size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse byte size %q: %w", raw, err)
	}
	if n < 0 {
		return 0, errors.New("byte size must be >= 0")
	}
	return int64(n * float64(mult)), nil
}
