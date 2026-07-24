// Package secrets resolves backend credentials and repository passwords from a
// runtime command. Stores retain values in memory only; this package never writes
// them to the cache, logs, or its own error strings, while callers own subsequent
// transport and persistence. Redactor masks known values in text passed to it.
// This is the highest-risk package in resticscope; treat it accordingly.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Material contains a repository's backend environment secrets and restic
// password. Env is nil for credential-free backends.
type Material struct {
	Env            map[string]string
	ResticPassword string
}

// credEntry accepts either generic backend environment values or the S3
// access-key shorthand. Validation rejects mixed forms to prevent shadowing.
type credEntry struct {
	AccessKey string            `json:"access_key"`
	SecretKey string            `json:"secret_key"`
	Env       map[string]string `json:"env"`
}

// envMap converts the S3 shorthand to AWS environment names and merges generic
// values after validation has excluded mixed forms.
func (c credEntry) envMap() map[string]string {
	out := make(map[string]string, len(c.Env)+2)
	if c.AccessKey != "" {
		out["AWS_ACCESS_KEY_ID"] = c.AccessKey
	}
	if c.SecretKey != "" {
		out["AWS_SECRET_ACCESS_KEY"] = c.SecretKey
	}
	maps.Copy(out, c.Env)
	return out
}

type repoEntry struct {
	ResticPassword string `json:"restic_password"`
}

// document is shared by JSON parsing and template generation.
type document struct {
	Credentials map[string]credEntry `json:"credentials"`
	Repos       map[string]repoEntry `json:"repos"`
}

// Store holds parsed secrets in memory. The zero value is not usable; build one
// with Parse or Load.
type Store struct {
	credentials map[string]credEntry
	repos       map[string]repoEntry
}

// RunFunc executes a secrets command through the given shell. Returned errors
// must not contain command output or secret material.
type RunFunc func(ctx context.Context, shell, command string) (stdout, stderr []byte, err error)

// Load runs a secrets command and parses its output. On runner failure it
// discards stdout and stderr because no Redactor exists yet and either stream may
// contain secret material, then wraps the runner's payload-free error.
func Load(ctx context.Context, run RunFunc, shell, command string) (*Store, error) {
	stdout, _, err := run(ctx, shell, command)
	if err != nil {
		return nil, fmt.Errorf("secrets_command failed: %w (stderr suppressed; it may contain secrets — run the command manually to diagnose)", err)
	}
	return Parse(stdout)
}

// Parse decodes the secrets JSON document. It does not validate against config;
// call Validate for that.
func Parse(data []byte) (*Store, error) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		// Never include data in the error: it is the secret payload.
		return nil, fmt.Errorf("parse secrets JSON: %w", err)
	}
	return &Store{credentials: doc.Credentials, repos: doc.Repos}, nil
}

// TemplateCred selects a named credential's S3 shorthand or generic environment
// scaffold.
type TemplateCred struct {
	Name string
	S3   bool
}

// Template returns a deterministic blank document accepted by Parse. It reads no
// secrets and emits empty values for users to fill, choosing either S3 shorthand
// or generic environment fields per credential.
func Template(creds []TemplateCred, repoNames []string) ([]byte, error) {
	type envCred struct {
		Env map[string]string `json:"env"`
	}
	type s3Cred struct {
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	doc := struct {
		Credentials map[string]any       `json:"credentials"`
		Repos       map[string]repoEntry `json:"repos"`
	}{
		Credentials: make(map[string]any, len(creds)),
		Repos:       make(map[string]repoEntry, len(repoNames)),
	}
	for _, c := range creds {
		if c.S3 {
			doc.Credentials[c.Name] = s3Cred{}
		} else {
			doc.Credentials[c.Name] = envCred{Env: map[string]string{}}
		}
	}
	for _, n := range repoNames {
		doc.Repos[n] = repoEntry{}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Validate requires complete wanted credentials and repositories. Extra entries
// produce warnings, while missing or incomplete entries produce errors; messages
// never contain secret values.
func (s *Store) Validate(wantCreds, wantRepos []string) (warnings []string, err error) {
	var errs []error

	wantCredSet := make(map[string]bool, len(wantCreds))
	for _, name := range wantCreds {
		wantCredSet[name] = true
		c, ok := s.credentials[name]
		if !ok {
			errs = append(errs, fmt.Errorf("secrets: credential %q missing from credentials map", name))
			continue
		}
		errs = append(errs, validateCredEntry(name, c)...)
	}

	wantRepoSet := make(map[string]bool, len(wantRepos))
	for _, name := range wantRepos {
		wantRepoSet[name] = true
		r, ok := s.repos[name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("secrets: repo %q missing from repos map", name))
		case r.ResticPassword == "":
			errs = append(errs, fmt.Errorf("secrets: repo %q missing restic_password", name))
		}
	}

	for name := range s.credentials {
		if !wantCredSet[name] {
			warnings = append(warnings, fmt.Sprintf("secrets: credential %q has no matching config; ignored", name))
		}
	}
	for name := range s.repos {
		if !wantRepoSet[name] {
			warnings = append(warnings, fmt.Sprintf("secrets: repo %q has no matching config; ignored", name))
		}
	}
	// Sort for a deterministic warning order: the loops above range over maps.
	slices.Sort(warnings)

	return warnings, errors.Join(errs...)
}

// validateCredEntry requires one complete credential form with valid,
// non-reserved names and non-empty values. Errors include names but not values.
func validateCredEntry(name string, c credEntry) []error {
	var errs []error
	shorthand := c.AccessKey != "" || c.SecretKey != ""
	switch {
	case shorthand && len(c.Env) > 0:
		errs = append(errs, fmt.Errorf("secrets: credential %q mixes access_key/secret_key with env; use one shape", name))
	case shorthand && (c.AccessKey == "" || c.SecretKey == ""):
		errs = append(errs, fmt.Errorf("secrets: credential %q missing access_key or secret_key", name))
	case !shorthand && len(c.Env) == 0:
		errs = append(errs, fmt.Errorf("secrets: credential %q provides no secrets (set access_key/secret_key or env)", name))
	}
	for _, k := range sortedEnvNames(c.Env) {
		switch {
		case !model.ValidBackendEnvName(k):
			errs = append(errs, fmt.Errorf("secrets: credential %q: env name %q is not a valid environment variable name", name, k))
		case model.ReservedBackendEnvName(k):
			errs = append(errs, fmt.Errorf("secrets: credential %q: env name %q is reserved by resticscope", name, k))
		case c.Env[k] == "":
			errs = append(errs, fmt.Errorf("secrets: credential %q: env %q must not be empty", name, k))
		}
	}
	return errs
}

func sortedEnvNames(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// Resolve returns repoName's password and credName's backend environment. An
// empty credName returns only the password; missing names fail without exposing
// values.
func (s *Store) Resolve(repoName, credName string) (Material, error) {
	var env map[string]string
	if credName != "" {
		c, ok := s.credentials[credName]
		if !ok {
			return Material{}, fmt.Errorf("secrets: no credential %q", credName)
		}
		env = c.envMap()
	}
	r, ok := s.repos[repoName]
	if !ok {
		return Material{}, fmt.Errorf("secrets: no repo %q", repoName)
	}
	return Material{
		Env:            env,
		ResticPassword: r.ResticPassword,
	}, nil
}
