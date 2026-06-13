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

	"resticscope/internal/theme"
)

// Config is the fully parsed, normalized, validated configuration.
//
// There is no separate credentials section: a repo names the secret env-var set
// it needs with `credential = "..."` (see Repo.Credential), and that reference
// is the declaration. CredentialNames derives the set of names the
// secrets_command must provide. Repos are decoded from their `[repos.<name>]`
// tables and assembled in Decode — see the Repos field.
type Config struct {
	Global  Global  `toml:"global"`
	Browse  Browse  `toml:"browse"`
	Diff    Diff    `toml:"diff"`
	Extract Extract `toml:"extract"`
	Theme   Theme   `toml:"theme"`

	// Repos is the file-ordered repo list. It is not decoded directly (hence
	// `toml:"-"`): TOML yields the `[repos.<name>]` tables as an unordered map,
	// so Decode assembles this slice in file order and resolves profile
	// inheritance before any other code sees it.
	Repos []Repo `toml:"-"`
}

// Theme selects the TUI color theme: one of the built-in palettes compiled
// into the binary (internal/theme — no theme files ship beside it), optionally
// adjusted per role through [theme.colors]. Overriding every role on top of
// any base yields a fully custom theme. Name is seeded to theme.DefaultName in
// Decode (so an explicit `name = ""` survives to be rejected); name and colors
// are checked in Validate so a typo'd theme or color fails at startup instead
// of rendering black.
//
// Background controls whether the TUI paints the terminal's default
// background/foreground (OSC 11/10) with the theme's bg/fg while it runs —
// required for a theme to look right on a terminal with a different scheme
// (e.g. a light theme on a dark terminal). Default true; set false to keep the
// terminal's own background (transparency, a matching terminal theme) and use
// only the foreground colors. Seeded true in Decode because a plain bool
// cannot distinguish an absent key from an explicit `false` afterward.
type Theme struct {
	Name       string      `toml:"name"`
	Background bool        `toml:"background"`
	Colors     ThemeColors `toml:"colors"`
}

// ThemeColors holds optional per-role color overrides applied on top of the
// named base palette. An empty field keeps the base color. Values are "#rgb" /
// "#rrggbb" hex, an ANSI-256 code "0"–"255" (which inherits the terminal's
// own palette for that slot), or the keyword "default" (the terminal's
// default color, rendered unstyled — the basis of the built-in "terminal"
// theme). For Bg/Fg the ANSI and "default" forms additionally skip the
// terminal-default painting of that channel even when Background is true,
// because OSC 10/11 take a concrete color, not a palette index — the
// terminal's existing default already is that color. The role vocabulary is
// documented on theme.Palette.
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

// Palette resolves the configured theme to the concrete palette the TUI
// renders with: the named built-in base with every non-empty override applied
// on top. An unknown name falls back to the default palette so a Config that
// skipped Normalize/Validate (tests, zero values) still renders sanely.
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

// The allowed [extract] unsafe_symlinks policy values. Validate rejects
// anything else; the app's metadata pass and the TUI's success-screen warning
// both branch on these constants, so the enum lives here with the field.
const (
	UnsafeSymlinksKeep        = "keep"        // leave verbatim + warn (default, restic-faithful)
	UnsafeSymlinksSkip        = "skip"        // remove from the output
	UnsafeSymlinksPlaceholder = "placeholder" // replace with an inert text file recording the target
)

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

	// RememberTarget keeps the target root the last extract run dispatched with
	// as the default for the next extract, for the lifetime of one TUI process
	// (in memory only — never persisted). Default true; seeded pre-decode like
	// every default-true bool so an explicit `false` stays distinguishable.
	RememberTarget bool `toml:"remember_target"`
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

// Repo is a single restic repository plus the expected_frequency that drives
// its freshness status. Where it lives is described one of two ways, exactly
// one per repo:
//
//   - url: the restic repository string, verbatim, for any backend restic
//     supports — "/srv/restic-repo", "sftp:user@host:/srv/restic-repo",
//     "rest:https://host:8000/repo", "b2:bucket:path", "azure:container:/",
//     "gs:bucket:/", "swift:container:/", "rclone:remote:path", or a
//     spelled-out "s3:..." URL. A leading ~ is expanded against $HOME.
//   - the s3 shorthand: endpoint (+ optional region and bucket_lookup) and
//     bucket (+ optional path), from which resticscope assembles the s3 URL.
//     Endpoint/region/bucket_lookup live here, not on the credential, so one
//     key pair can back buckets in several regions.
//
// credential names the secret env-var set restic gets for this repo; it is
// optional because some backends need none (local, sftp via ssh config or
// agent, rclone via its own config). env carries non-secret backend env vars
// (e.g. GOOGLE_PROJECT_ID, AZURE_ACCOUNT_NAME — secrets stay in the
// credential); options carries restic -o backend options verbatim (e.g.
// "sftp.command", "rest.connections").
type Repo struct {
	// Name is the final component of the repo's table header (`[repos.<name>]`),
	// assigned in Decode — a literal `name = ...` inside a repo table is rejected.
	// Profile names a `[profiles.<name>]` table whose fields this repo inherits
	// (optional); it is resolved away in Decode, leaving every field merged.
	Name       string `toml:"name"`
	Profile    string `toml:"profile"`
	Credential string `toml:"credential"`

	// Generic form: the restic repository string, any backend.
	URL string `toml:"url"`

	// Non-secret backend env vars and -o options, passed to restic verbatim.
	Env     map[string]string `toml:"env"`
	Options map[string]string `toml:"options"`

	// S3 shorthand. endpoint/bucket(+path) assemble the s3 URL, region becomes
	// AWS_DEFAULT_REGION, bucket_lookup becomes -o s3.bucket-lookup.
	Endpoint     string `toml:"endpoint"`
	Region       string `toml:"region"`
	Bucket       string `toml:"bucket"`
	Path         string `toml:"path"`
	BucketLookup string `toml:"bucket_lookup"` // auto | dns | path

	ExpectedFrequency Duration          `toml:"expected_frequency"`
	Labels            map[string]string `toml:"labels"`
}

// knownBackendSchemes are the repository-string schemes restic understands.
// Validate checks an explicit url against this set so a typo'd scheme fails at
// startup instead of as a cryptic restic error mid-refresh.
var knownBackendSchemes = map[string]bool{
	"local": true, "sftp": true, "rest": true, "s3": true, "swift": true,
	"b2": true, "azure": true, "gs": true, "rclone": true,
}

// RepositoryURL returns the restic repository string (RESTIC_REPOSITORY) for
// the repo: url verbatim when set, otherwise the URL assembled from the s3
// shorthand. The endpoint scheme is preserved — restic needs https:// to talk
// to non-AWS endpoints like Hetzner (plan §7). Assembled form:
// s3:https://host[:port]/bucket[/path].
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

// Backend returns the repo's restic backend scheme ("s3", "sftp", ...), or
// "local" for a bare filesystem path. Display-only; derived from the
// repository string so the two can never disagree.
func (r Repo) Backend() string {
	if scheme, _, ok := strings.Cut(r.RepositoryURL(), ":"); ok && knownBackendSchemes[scheme] {
		return scheme
	}
	return "local"
}

// BackendOptions returns every restic -o option for the repo: the options map
// plus the s3 shorthand's bucket_lookup (dns/path only — auto is restic's
// default and is not emitted). Returns nil when there are none.
func (r Repo) BackendOptions() map[string]string {
	var out map[string]string
	if len(r.Options) > 0 {
		out = make(map[string]string, len(r.Options)+1)
		for k, v := range r.Options {
			out[k] = v
		}
	}
	if r.URL == "" && (r.BucketLookup == "dns" || r.BucketLookup == "path") {
		if out == nil {
			out = make(map[string]string, 1)
		}
		out["s3.bucket-lookup"] = r.BucketLookup
	}
	return out
}

// BackendEnv returns the repo's non-secret backend env vars: the env map plus
// the s3 shorthand's region as AWS_DEFAULT_REGION (omitted when empty — the
// endpoint host usually implies it, and restic must not see an empty value).
// Returns nil when there are none. Secret env vars come from the credential
// (internal/secrets), never from here.
func (r Repo) BackendEnv() map[string]string {
	var out map[string]string
	if len(r.Env) > 0 {
		out = make(map[string]string, len(r.Env)+1)
		for k, v := range r.Env {
			out[k] = v
		}
	}
	if r.URL == "" && r.Region != "" {
		if out == nil {
			out = make(map[string]string, 1)
		}
		out["AWS_DEFAULT_REGION"] = r.Region
	}
	return out
}

// CredentialNames returns, in first-reference order, the distinct credential
// names the repos use. Credentials are not declared separately: a repo names the
// secret env-var set it needs with `credential = "..."`, and that reference is
// the declaration. The result drives the secrets-template scaffold and the
// secrets completeness check, where each name must resolve to material the
// secrets_command provides.
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
