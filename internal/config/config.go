// Package config loads and validates resticscope's TOML configuration. It
// contains repository metadata but never credentials or restic passwords,
// which come from secrets_command through internal/secrets.
package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/theme"
)

// Config is a parsed, normalized, validated configuration containing no
// credentials. There is no credentials section: a repo's `credential = "..."`
// reference is the declaration, and CredentialNames derives the set the
// secrets_command must provide.
type Config struct {
	Global  Global  `toml:"global"`
	Browse  Browse  `toml:"browse"`
	Diff    Diff    `toml:"diff"`
	Extract Extract `toml:"extract"`
	Theme   Theme   `toml:"theme"`

	// Repos lists repositories in file order after profile inheritance is resolved.
	// It is `toml:"-"` because TOML yields the `[repos.<name>]` tables unordered,
	// so Decode assembles the slice itself.
	Repos []Repo `toml:"-"`
}

// Theme selects a built-in TUI palette with optional per-role overrides.
// Background defaults to true and controls OSC 11/10 painting of terminal
// defaults. Disabling it retains terminal transparency or an existing matching
// theme while palette foreground colors still apply. Invalid names and colors
// fail validation.
type Theme struct {
	Name       string      `toml:"name"`
	Background bool        `toml:"background"`
	Colors     ThemeColors `toml:"colors"`
}

// ThemeColors contains optional role overrides; empty fields retain base colors.
// Values accept "#rgb", "#rrggbb", ANSI-256 indexes "0"-"255", or "default".
// For Bg and Fg, ANSI and default values also disable OSC terminal-default
// painting. See theme.Palette for role semantics.
type ThemeColors struct {
	Bg     string `toml:"bg"`
	Fg     string `toml:"fg"`
	Grey   string `toml:"grey"`
	Dim    string `toml:"dim"`
	Red    string `toml:"red"`
	Green  string `toml:"green"`
	Yellow string `toml:"yellow"`
	Blue   string `toml:"blue"`
	Aqua   string `toml:"aqua"`
	Orange string `toml:"orange"`
}

// Palette resolves the named base theme with non-empty overrides applied.
// Unknown names use the default palette so zero-value or unvalidated
// configurations still render.
func (t Theme) Palette() theme.Palette {
	p, ok := theme.Lookup(t.Name)
	if !ok {
		p = theme.Default()
	}
	o := t.Colors
	apply := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	apply(&p.Bg, o.Bg)
	apply(&p.Fg, o.Fg)
	apply(&p.Grey, o.Grey)
	apply(&p.Dim, o.Dim)
	apply(&p.Red, o.Red)
	apply(&p.Green, o.Green)
	apply(&p.Yellow, o.Yellow)
	apply(&p.Blue, o.Blue)
	apply(&p.Aqua, o.Aqua)
	apply(&p.Orange, o.Orange)
	return p
}

// Browse configures snapshot browsing. IndexTimeout caps namespace indexing;
// MaxDiskBytes caps encrypted browse storage for the process, with zero meaning
// unlimited.
type Browse struct {
	IndexTimeout Duration `toml:"index_timeout"`
	MaxDiskBytes ByteSize `toml:"max_disk_bytes"`
}

// Diff configures snapshot diffing with an independent timeout for operations
// that can outlast global restic probes.
type Diff struct {
	Timeout Duration `toml:"timeout"`
}

// The UnsafeSymlinks constants define values accepted by Validate.
const (
	UnsafeSymlinksKeep        = "keep"        // preserve the link and warn (default; matches restic)
	UnsafeSymlinksSkip        = "skip"        // omit the link from output
	UnsafeSymlinksPlaceholder = "placeholder" // replace the link with a text file containing its target
)

// Extract configures local extraction through read-only restic invocations.
// TargetRoot is normalized to an expanded absolute base directory, and
// ExtractTimeout caps each restore.
type Extract struct {
	TargetRoot     string   `toml:"target_root"`
	ExtractTimeout Duration `toml:"extract_timeout"`

	// UnsafeSymlinks controls absolute or escaping restored links. Decode defaults
	// it to UnsafeSymlinksKeep, and Validate accepts only those constants.
	UnsafeSymlinks string `toml:"unsafe_symlinks"`

	// RememberTarget reuses the last extract root for the process lifetime. It
	// defaults to true and is never persisted.
	RememberTarget bool `toml:"remember_target"`
}

// Global holds process-wide settings.
type Global struct {
	Parallelism       int      `toml:"parallelism"` // Parallelism limits concurrent repository checks and refreshes; it defaults to 4.
	CacheDir          string   `toml:"cache_dir"`
	LogFile           string   `toml:"log_file"`
	RefreshOnOpen     bool     `toml:"refresh_on_open"`     // RefreshOnOpen refreshes never-seen or stale repositories when the TUI opens; it defaults to true.
	Shell             string   `toml:"shell"`               // Shell selects the interpreter for SecretsCommand and interactive sessions; empty uses $SHELL, then /bin/sh.
	ShellPasswordMode string   `toml:"shell_password_mode"` // ShellPasswordMode is "file" (default) for a private temporary file or "env" for RESTIC_PASSWORD.
	SecretsCommand    string   `toml:"secrets_command"`     // SecretsCommand runs through Shell and must emit one secrets JSON document on stdout.
	GroupBy           []string `toml:"group_by"`            // label keys available for grouping in order; empty disables it

	// Duration fields accept Go duration strings such as "10m" or "24h".
	StaleAfter            Duration `toml:"stale_after"`             // cache entries older than this are refreshed on open
	StaleGrace            Duration `toml:"stale_grace"`             // amber band added on top of a repo's expected_frequency
	LockMaxAge            Duration `toml:"lock_max_age"`            // a lock older than this is treated as red
	SecretsCommandTimeout Duration `toml:"secrets_command_timeout"` // timeout for secrets_command
	ResticCommandTimeout  Duration `toml:"restic_command_timeout"`  // timeout for each restic invocation
}

// Repo describes one restic repository and its freshness expectations. It uses
// either URL verbatim, with a leading ~ expanded, or S3 shorthand fields. S3
// endpoint and region belong to the repository so one credential can span
// regions. Credential names an optional secret environment set; Env remains
// non-secret, and Options passes restic -o values verbatim.
type Repo struct {
	// Name comes from the final repo table component; a literal name field is
	// rejected. Profile optionally names inherited settings and is resolved by Decode.
	Name       string `toml:"name"`
	Profile    string `toml:"profile"`
	Credential string `toml:"credential"`

	// URL is the restic repository string for any backend.
	URL string `toml:"url"`

	// Env holds non-secret backend variables; Options holds restic -o values.
	Env     map[string]string `toml:"env"`
	Options map[string]string `toml:"options"`

	// S3 shorthand fields assemble the URL, AWS_DEFAULT_REGION, and bucket lookup.
	Endpoint     string `toml:"endpoint"`
	Region       string `toml:"region"`
	Bucket       string `toml:"bucket"`
	Path         string `toml:"path"`
	BucketLookup string `toml:"bucket_lookup"` // auto | dns | path

	ExpectedFrequency Duration          `toml:"expected_frequency"`
	Labels            map[string]string `toml:"labels"`
}

// knownBackendSchemes lists repository URL schemes accepted by Validate.
var knownBackendSchemes = map[string]bool{
	"local": true, "sftp": true, "rest": true, "s3": true, "swift": true,
	"b2": true, "azure": true, "gs": true, "rclone": true,
}

// RepositoryURL returns URL verbatim when set or assembles an S3 URL from
// shorthand fields. It preserves endpoint schemes required by non-AWS HTTPS
// endpoints.
func (r Repo) RepositoryURL() string {
	if r.URL != "" {
		return r.URL
	}
	endpoint := strings.TrimRight(r.Endpoint, "/")
	u := "s3:" + endpoint + "/" + r.Bucket
	if p := strings.Trim(r.Path, "/"); p != "" {
		u += "/" + p
	}
	return u
}

// Backend returns the restic backend scheme, or "local" for a bare filesystem
// path. It is derived from RepositoryURL for display.
func (r Repo) Backend() string {
	if scheme, _, ok := strings.Cut(r.RepositoryURL(), ":"); ok && knownBackendSchemes[scheme] {
		return scheme
	}
	return "local"
}

// BackendOptions returns Options plus a dns or path S3 bucket lookup. The auto
// value is omitted so restic uses its default; the result is nil when empty.
func (r Repo) BackendOptions() map[string]string {
	var out map[string]string
	if len(r.Options) > 0 {
		out = make(map[string]string, len(r.Options)+1)
		maps.Copy(out, r.Options)
	}
	if r.URL == "" && (r.BucketLookup == "dns" || r.BucketLookup == "path") {
		if out == nil {
			out = make(map[string]string, 1)
		}
		out["s3.bucket-lookup"] = r.BucketLookup
	}
	return out
}

// BackendEnv returns Env plus a non-empty S3 region as AWS_DEFAULT_REGION. The
// result is nil when empty; secret variables come only from internal/secrets.
func (r Repo) BackendEnv() map[string]string {
	var out map[string]string
	if len(r.Env) > 0 {
		out = make(map[string]string, len(r.Env)+1)
		maps.Copy(out, r.Env)
	}
	if r.URL == "" && r.Region != "" {
		if out == nil {
			out = make(map[string]string, 1)
		}
		out["AWS_DEFAULT_REGION"] = r.Region
	}
	return out
}

// CredentialNames returns distinct credential names in first-reference order.
// Repo references act as declarations and drive secret-template generation and
// completeness checks.
func (c *Config) CredentialNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, r := range c.Repos {
		if r.Credential != "" && !seen[r.Credential] {
			seen[r.Credential] = true
			names = append(names, r.Credential)
		}
	}
	return names
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

// ByteSize is a byte count parsed from TOML text with optional binary units.
type ByteSize int64

// UnmarshalText parses binary and abbreviated byte-size units. It rejects
// negative, unparseable, and int64-overflowing values.
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

// parseByteSize parses bare byte counts and K/KB/KiB, M/MB/MiB, or G/GB/GiB
// suffixes as binary units. It rejects negative values.
func parseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, errors.New("empty byte size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"gib", 1 << 30},
		{"gb", 1 << 30},
		{"g", 1 << 30},
		{"mib", 1 << 20},
		{"mb", 1 << 20},
		{"m", 1 << 20},
		{"kib", 1 << 10},
		{"kb", 1 << 10},
		{"k", 1 << 10},
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
	// Guard conversion because out-of-range floats may wrap; float64(MaxInt64)
	// rounds to 2^63.
	bytes := n * float64(mult)
	if math.IsNaN(bytes) || bytes >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("byte size %q is out of range", raw)
	}
	return int64(bytes), nil
}
