// Package config loads and validates resticscope's TOML configuration.
//
// It never holds secrets, since credentials and restic passwords come from the
// secrets_command (see internal/secrets), not from this file. Config only
// describes which repos exist and what is expected of them.
package config

import (
	"errors"
	"fmt"
	"math"
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
	Diff        Diff         `toml:"diff"`
	Extract     Extract      `toml:"extract"`
}

// Browse bounds the in-app snapshot file browser. The first time a snapshot is
// browsed its whole namespace is streamed once into a session-scoped,
// encrypted-at-rest SQLite DB, and all later navigation is served from SQL.
// IndexTimeout caps how long that one-time index may run (generous, because a
// huge snapshot can take minutes). MaxDiskBytes is an optional session-wide
// ceiling on the total size of the encrypted browse directory — 0 means
// unlimited — enforced by the browse store across every repo/snapshot indexed
// during the run, not by resticx. Filenames are persisted only in that encrypted
// DB and destroyed on clean exit.
type Browse struct {
	IndexTimeout Duration `toml:"index_timeout"`
	MaxDiskBytes ByteSize `toml:"max_disk_bytes"`
}

// Diff bounds the in-app snapshot diff stream. Diffing two large snapshots can
// legitimately take longer than cheap restic probes such as snapshots/cat, so it
// has its own timeout instead of sharing global.restic_command_timeout.
type Diff struct {
	Timeout Duration `toml:"timeout"`
}

// Extract bounds the in-app extract feature, which copies a file, directory, or
// (later) full snapshot out of a backup with a read-only restic invocation and
// writes it to the local filesystem. TargetRoot is the base directory for every
// extract's per-op output subdir; it is stored expanded and absolute after
// Normalize/Validate. ExtractTimeout caps a single extract run — extracting a
// large tree can take minutes, so it has its own timeout instead
// of sharing global.restic_command_timeout.
type Extract struct {
	TargetRoot     string   `toml:"target_root"`
	ExtractTimeout Duration `toml:"extract_timeout"`

	// UnsafeSymlinks is the policy for a restored symlink whose target is absolute
	// or escapes the extracted tree (and so would alias the live filesystem):
	// "keep" (default; leave verbatim + warn, matching restic), "skip" (remove from
	// the output), or "placeholder" (replace with an inert text file recording the
	// target). Validated against that enum; defaulted in Decode.
	UnsafeSymlinks string `toml:"unsafe_symlinks"`
}

// Global holds process-wide settings.
type Global struct {
	Parallelism       int      `toml:"parallelism"`
	CacheDir          string   `toml:"cache_dir"`
	LogFile           string   `toml:"log_file"`
	RefreshOnOpen     bool     `toml:"refresh_on_open"`
	Shell             string   `toml:"shell"`
	ShellPasswordMode string   `toml:"shell_password_mode"`
	SecretsCommand    string   `toml:"secrets_command"`
	GroupBy           []string `toml:"group_by"` // repo label keys the list view can group by; `g` cycles through them and a flat view, in list order. Empty = no grouping.

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
// rejects negative, unparseable, or int64-overflowing values.
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
	// Guard the float64→int64 conversion. Go does NOT saturate on an out-of-range
	// float conversion — the result is implementation-defined (typically wraps to
	// math.MinInt64), so an absurd value like "9000000000G" would otherwise become
	// a negative or a finite-but-bogus cap rather than a clear error. float64 also
	// can only represent the product exactly up to 2^53, so multi-PiB sizes round;
	// that imprecision is irrelevant for a disk ceiling. float64(math.MaxInt64)
	// rounds up to 2^63, so anything >= it (including +Inf) cannot fit in int64.
	bytes := n * float64(mult)
	if math.IsNaN(bytes) || bytes >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("byte size %q is out of range", raw)
	}
	return int64(bytes), nil
}
