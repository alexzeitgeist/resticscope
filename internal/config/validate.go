package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"resticscope/internal/model"
	"resticscope/internal/theme"
)

var validBucketLookup = map[string]bool{"auto": true, "dns": true, "path": true}

// validRepoName restricts repo names to characters that survive unchanged
// through cache-file and restic-cache-dir sanitization. Without this, distinct
// names like "foo/bar" and "foo:bar" would both collapse to "foo_bar" and share
// (and clobber) each other's on-disk state.
var validRepoName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the normalized config for structural problems: missing
// required fields, duplicate or dangling names, and bad enum values. It does
// not contact the backend, restic, or the secrets_command.
func (c *Config) Validate() error {
	var errs []error

	if c.Global.SecretsCommand == "" {
		errs = append(errs, errors.New("global.secrets_command is required"))
	}
	if m := c.Global.ShellPasswordMode; m != "file" && m != "env" {
		errs = append(errs, fmt.Errorf("global.shell_password_mode must be \"file\" or \"env\", got %q", m))
	}

	seenGroupBy := map[string]int{}
	for i, k := range c.Global.GroupBy {
		if k == "" {
			errs = append(errs, fmt.Errorf("global.group_by[%d]: key must not be empty", i))
			continue
		}
		norm := strings.TrimSpace(k)
		if norm != k {
			errs = append(errs, fmt.Errorf("global.group_by[%d]: key %q has surrounding whitespace", i, k))
		}
		// Dedup on the trimmed key so [" env", "env"] surfaces as a duplicate
		// rather than slipping through under two different map entries.
		if prev, ok := seenGroupBy[norm]; ok {
			errs = append(errs, fmt.Errorf("global.group_by[%d]: duplicate key %q (also at index %d)", i, k, prev))
		} else {
			seenGroupBy[norm] = i
		}
	}

	credNames := map[string]bool{}
	for i, cr := range c.Credentials {
		switch {
		case cr.Name == "":
			errs = append(errs, fmt.Errorf("credentials[%d]: name is required", i))
		case credNames[cr.Name]:
			errs = append(errs, fmt.Errorf("duplicate credential name %q", cr.Name))
		default:
			credNames[cr.Name] = true
		}
	}

	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("no repos configured"))
	}
	repoNames := map[string]bool{}
	for i, r := range c.Repos {
		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("repos[%d]: name is required", i))
		case repoNames[r.Name]:
			errs = append(errs, fmt.Errorf("duplicate repo name %q", r.Name))
		default:
			repoNames[r.Name] = true
		}
		if r.Name != "" && !validRepoName.MatchString(r.Name) {
			errs = append(errs, fmt.Errorf("repo %q: name may contain only letters, digits, '.', '_' and '-' (it becomes a cache filename)", r.Name))
		}
		errs = append(errs, validateRepoLocation(r)...)
		// credential is optional: local/sftp/rclone backends need no secret env
		// vars. When set it must resolve, as before.
		if r.Credential != "" && !credNames[r.Credential] {
			errs = append(errs, fmt.Errorf("repo %q: credential %q does not match any [[credentials]] block", r.Name, r.Credential))
		}
		errs = append(errs, validateRepoEnvOptions(r)...)
		if r.ExpectedFrequency <= 0 {
			errs = append(errs, fmt.Errorf("repo %q: expected_frequency must be a positive duration (e.g. \"24h\")", r.Name))
		}
	}

	errs = append(errs, c.validateBrowse()...)
	errs = append(errs, c.validateDiff()...)
	errs = append(errs, c.validateExtract()...)
	errs = append(errs, c.validateTheme()...)

	return errors.Join(errs...)
}

// validateRepoLocation checks that the repo describes where it lives in
// exactly one of the two supported forms: a generic restic repository url, or
// the s3 shorthand (endpoint/bucket + optional region/path/bucket_lookup).
// Mixing them is rejected — the shorthand fields would be silently ignored
// otherwise, which always means the user misunderstood one of the forms.
//
// A url is accepted when it is a known restic scheme ("scheme:rest") or a bare
// absolute filesystem path (restic's local backend); an unknown scheme or a
// relative path is rejected here so the typo fails at startup rather than as a
// restic error mid-refresh. Normalize has already expanded a leading ~.
func validateRepoLocation(r Repo) []error {
	var errs []error
	if r.URL == "" {
		// s3 shorthand: same required fields as always.
		if r.Bucket == "" {
			errs = append(errs, fmt.Errorf("repo %q: bucket is required (or set url for a non-s3 backend)", r.Name))
		}
		if r.Endpoint == "" {
			errs = append(errs, fmt.Errorf("repo %q: endpoint is required (or set url for a non-s3 backend)", r.Name))
		}
		if !validBucketLookup[r.BucketLookup] {
			errs = append(errs, fmt.Errorf("repo %q: bucket_lookup must be auto|dns|path, got %q", r.Name, r.BucketLookup))
		}
		return errs
	}
	if r.Endpoint != "" || r.Region != "" || r.Bucket != "" || r.Path != "" || r.BucketLookup != "" {
		errs = append(errs, fmt.Errorf("repo %q: url and the s3 shorthand fields (endpoint/region/bucket/path/bucket_lookup) are mutually exclusive", r.Name))
	}
	if scheme, _, ok := strings.Cut(r.URL, ":"); ok && schemeLike(scheme) {
		if !knownBackendSchemes[scheme] {
			errs = append(errs, fmt.Errorf("repo %q: url scheme %q is not a restic backend (one of: %s, or a bare absolute path)", r.Name, scheme, strings.Join(sortedKeys(knownBackendSchemes), ", ")))
		}
	} else if !filepath.IsAbs(r.URL) {
		errs = append(errs, fmt.Errorf("repo %q: url must be a restic repository (scheme:...) or an absolute path", r.Name))
	}
	return errs
}

// schemeLike reports whether s looks like a URL scheme rather than the start
// of a path — lowercase letters/digits only, as restic's schemes are. A path
// like "/srv/x" or "C" fails this and is judged as a filesystem path instead.
func schemeLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// validateRepoEnvOptions checks the repo's generic backend env/options maps:
// env names must be valid, non-reserved env identifiers (the model contract
// resticx enforces at assembly time), env values and option keys must be
// non-empty. Values are user-chosen but non-secret, so error messages may name
// the key; they still never echo the value.
func validateRepoEnvOptions(r Repo) []error {
	var errs []error
	for _, k := range sortedKeys(r.Env) {
		switch {
		case !model.ValidBackendEnvName(k):
			errs = append(errs, fmt.Errorf("repo %q: env name %q is not a valid environment variable name", r.Name, k))
		case model.ReservedBackendEnvName(k):
			errs = append(errs, fmt.Errorf("repo %q: env name %q is reserved by resticscope", r.Name, k))
		case r.Env[k] == "":
			errs = append(errs, fmt.Errorf("repo %q: env %q must not be empty", r.Name, k))
		}
	}
	for _, k := range sortedKeys(r.Options) {
		if k == "" {
			errs = append(errs, fmt.Errorf("repo %q: options keys must not be empty", r.Name))
		}
	}
	return errs
}

// sortedKeys returns m's keys sorted, for deterministic multi-error output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateTheme checks the [theme] block: the name must be a built-in theme
// (the message lists every valid choice, since the set lives in the binary and
// is otherwise undiscoverable), and each non-empty [theme.colors] override
// must be a color lipgloss can parse — rejected here by theme.ValidColor so a
// typo fails at startup instead of silently rendering as black. Name is seeded
// to the default in Decode, so an empty value reaching here was explicit and
// is rejected like any other unknown name.
func (c *Config) validateTheme() []error {
	var errs []error
	if _, ok := theme.Lookup(c.Theme.Name); !ok {
		errs = append(errs, fmt.Errorf("theme.name %q is not a built-in theme (one of: %s)",
			c.Theme.Name, strings.Join(theme.Names(), ", ")))
	}
	o := c.Theme.Colors
	roles := []struct{ key, value string }{
		{"bg", o.Bg}, {"fg", o.Fg}, {"grey", o.Grey}, {"dim", o.Dim},
		{"red", o.Red}, {"green", o.Green}, {"yellow", o.Yellow},
		{"blue", o.Blue}, {"aqua", o.Aqua}, {"orange", o.Orange},
	}
	for _, r := range roles {
		if r.value != "" && !theme.ValidColor(r.value) {
			errs = append(errs, fmt.Errorf("theme.colors.%s: %q is not a hex color (\"#rgb\" / \"#rrggbb\"), an ANSI-256 code (\"0\"–\"255\"), or \"default\"", r.key, r.value))
		}
	}
	return errs
}

func (c *Config) validateDiff() []error {
	var errs []error
	if c.Diff.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("diff.timeout must be a positive duration, got %q", c.Diff.Timeout.Std()))
	}
	return errs
}

// validateExtract checks the extract output base and timeout. target_root is
// seeded in Decode and only ~-expanded in Normalize, so an omitted key arrives
// here as the default (absolute) path, while an explicit empty or relative value
// survives to be rejected as a config error. Error messages name the key and the
// kind of failure but never echo the user's path — the privacy discipline that
// governs extract source/destination paths starts at config time. extract_timeout
// is likewise seeded pre-decode, so a value reaching here at <= 0 was set
// explicitly and is rejected rather than silently defaulted.
func (c *Config) validateExtract() []error {
	var errs []error
	switch root := c.Extract.TargetRoot; {
	case root == "":
		errs = append(errs, errors.New("extract.target_root must not be empty"))
	case !filepath.IsAbs(root):
		errs = append(errs, errors.New("extract.target_root must be an absolute path (~ is expanded against $HOME)"))
	}
	if c.Extract.ExtractTimeout <= 0 {
		errs = append(errs, fmt.Errorf("extract.extract_timeout must be a positive duration, got %q", c.Extract.ExtractTimeout.Std()))
	}
	// unsafe_symlinks is seeded to "keep" in Decode, so an omitted key arrives
	// valid; an explicit empty/unknown value survives to be rejected here. The
	// message names the key and the allowed set but never echoes the bad value —
	// same path-free discipline as the rest of the extract config.
	switch c.Extract.UnsafeSymlinks {
	case UnsafeSymlinksKeep, UnsafeSymlinksSkip, UnsafeSymlinksPlaceholder:
	default:
		errs = append(errs, errors.New(`extract.unsafe_symlinks must be "keep", "skip", or "placeholder"`))
	}
	return errs
}

// validateBrowse checks the browse index settings: index_timeout must be a
// positive duration, and max_disk_bytes must be non-negative (0 = unlimited).
// index_timeout's default is seeded pre-decode (in Decode), so a value reaching
// here at <= 0 was set explicitly — an explicit `0` or a negative — and is
// rejected rather than silently defaulted.
func (c *Config) validateBrowse() []error {
	var errs []error
	b := c.Browse
	if b.IndexTimeout <= 0 {
		errs = append(errs, fmt.Errorf("browse.index_timeout must be a positive duration, got %q", b.IndexTimeout.Std()))
	}
	if b.MaxDiskBytes < 0 {
		errs = append(errs, fmt.Errorf("browse.max_disk_bytes must be >= 0, got %d", b.MaxDiskBytes.Bytes()))
	}
	return errs
}
