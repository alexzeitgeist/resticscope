package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/theme"
)

var validBucketLookup = map[string]bool{"auto": true, "dns": true, "path": true}

// validRepoName allows only characters preserved by both the state-file and
// restic-cache-directory sanitizers, preventing collisions that clobber state.
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
		// Deduplicate trimmed keys so whitespace variants cannot bypass validation.
		if prev, ok := seenGroupBy[norm]; ok {
			errs = append(errs, fmt.Errorf("global.group_by[%d]: duplicate key %q (also at index %d)", i, k, prev))
		} else {
			seenGroupBy[norm] = i
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
		// Credentials are optional names resolved against secrets_command output
		// when secrets are loaded, not during config validation.
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

// validateRepoLocation accepts exactly one of a generic URL or S3 shorthand.
// URLs use known schemes or absolute paths so typos fail before refresh, and
// mixed forms fail instead of silently ignoring shorthand fields.
func validateRepoLocation(r Repo) []error {
	var errs []error
	if r.URL == "" {
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

// schemeLike reports whether s matches restic's lowercase alphanumeric scheme
// syntax rather than a filesystem path.
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

func validateRepoEnvOptions(r Repo) []error {
	return validateEnvOptions(fmt.Sprintf("repo %q", r.Name), r.Env, r.Options)
}

// validateEnvOptions validates backend keys and non-empty values for repositories
// and profiles. Values are non-secret; errors may name keys but never values.
func validateEnvOptions(label string, env, options map[string]string) []error {
	var errs []error
	for _, k := range sortedKeys(env) {
		switch {
		case !model.ValidBackendEnvName(k):
			errs = append(errs, fmt.Errorf("%s: env name %q is not a valid environment variable name", label, k))
		case model.ReservedBackendEnvName(k):
			errs = append(errs, fmt.Errorf("%s: env name %q is reserved by resticscope", label, k))
		case env[k] == "":
			errs = append(errs, fmt.Errorf("%s: env %q must not be empty", label, k))
		}
	}
	for _, k := range sortedKeys(options) {
		switch {
		case k == "":
			errs = append(errs, fmt.Errorf("%s: options keys must not be empty", label))
		case options[k] == "":
			errs = append(errs, fmt.Errorf("%s: option %q must not be empty", label, k))
		}
	}
	return errs
}

// sortedKeys returns m's keys sorted, for deterministic multi-error output.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// validateTheme checks built-in names and non-empty color overrides. Diagnostics
// list compiled names and formats so invalid values fail at startup; explicit
// empty names remain invalid.
func (c *Config) validateTheme() []error {
	var errs []error
	if _, ok := theme.Lookup(c.Theme.Name); !ok {
		errs = append(errs, fmt.Errorf("theme.name %q is not a built-in theme (one of: %s)",
			c.Theme.Name, strings.Join(theme.Names(), ", ")))
	}
	o := c.Theme.Colors
	roles := []struct{ key, value string }{
		{"bg", o.Bg},
		{"fg", o.Fg},
		{"grey", o.Grey},
		{"dim", o.Dim},
		{"red", o.Red},
		{"green", o.Green},
		{"yellow", o.Yellow},
		{"blue", o.Blue},
		{"aqua", o.Aqua},
		{"orange", o.Orange},
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

// validateExtract checks the normalized output root and timeout without echoing
// paths. Decode seeding preserves explicit invalid values for rejection.
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
	// Decode seeding preserves explicit invalid policies for rejection without echoing them.
	switch c.Extract.UnsafeSymlinks {
	case UnsafeSymlinksKeep, UnsafeSymlinksSkip, UnsafeSymlinksPlaceholder:
	default:
		errs = append(errs, errors.New(`extract.unsafe_symlinks must be "keep", "skip", or "placeholder"`))
	}
	return errs
}

// validateBrowse requires a positive index timeout and nonnegative disk cap, with
// zero disk unlimited. Decode seeding preserves explicit invalid timeouts.
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
