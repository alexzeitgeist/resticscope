package config

import (
	"errors"
	"fmt"
	"regexp"
)

var validBucketLookup = map[string]bool{"auto": true, "dns": true, "path": true}

// validRepoName restricts repo names to characters that survive unchanged
// through cache-file and restic-cache-dir sanitization. Without this, distinct
// names like "foo/bar" and "foo:bar" would both collapse to "foo_bar" and share
// (and clobber) each other's on-disk state.
var validRepoName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the normalized config for structural problems: missing
// required fields, duplicate or dangling names, and bad enum values. It does
// not contact S3, restic, or the secrets_command.
func (c *Config) Validate() error {
	var errs []error

	if c.Global.SecretsCommand == "" {
		errs = append(errs, errors.New("global.secrets_command is required"))
	}
	if m := c.Global.ShellPasswordMode; m != "file" && m != "env" {
		errs = append(errs, fmt.Errorf("global.shell_password_mode must be \"file\" or \"env\", got %q", m))
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
		if r.Bucket == "" {
			errs = append(errs, fmt.Errorf("repo %q: bucket is required", r.Name))
		}
		if r.Endpoint == "" {
			errs = append(errs, fmt.Errorf("repo %q: endpoint is required", r.Name))
		}
		if !validBucketLookup[r.BucketLookup] {
			errs = append(errs, fmt.Errorf("repo %q: bucket_lookup must be auto|dns|path, got %q", r.Name, r.BucketLookup))
		}
		if r.Credential == "" {
			errs = append(errs, fmt.Errorf("repo %q: credential is required", r.Name))
		} else if !credNames[r.Credential] {
			errs = append(errs, fmt.Errorf("repo %q: credential %q does not match any [[credentials]] block", r.Name, r.Credential))
		}
		if r.ExpectedFrequency <= 0 {
			errs = append(errs, fmt.Errorf("repo %q: expected_frequency must be a positive duration (e.g. \"24h\")", r.Name))
		}
	}

	return errors.Join(errs...)
}
