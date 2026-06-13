// Package secrets resolves backend credentials and restic passwords at runtime
// by running the user's secrets_command and parsing its JSON output.
//
// Secrets live only in memory for the lifetime of the process. They are never
// written to the cache, logs, or error strings. The Redactor exists to ensure
// any text that might contain a secret (restic stderr, log lines) can be
// scrubbed before it leaves the process. This is the highest-risk package in
// resticscope; treat it accordingly.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"

	"resticscope/internal/model"
)

// Material is the resolved secret bundle for a single repo: the backend env
// vars from its credential (nil when the repo has no credential — local or
// sftp backends) plus the repo's own restic password.
type Material struct {
	Env            map[string]string
	ResticPassword string
}

// credEntry is one credential in the secrets JSON. env is the generic,
// backend-agnostic shape: the secret environment variables restic needs
// (B2_ACCOUNT_KEY, AZURE_ACCOUNT_KEY, RESTIC_REST_PASSWORD, ...).
// access_key/secret_key remain as the s3 shorthand — exactly equivalent to env
// entries AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY. A single entry uses one
// shape or the other; Validate rejects mixing so no key silently shadows
// another.
type credEntry struct {
	AccessKey string            `json:"access_key"`
	SecretKey string            `json:"secret_key"`
	Env       map[string]string `json:"env"`
}

// envMap flattens the entry to the env vars it provides, lowering the s3
// shorthand onto its AWS names. Validate guarantees the two shapes are not
// mixed, so there is no precedence question here.
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

// document is the on-the-wire shape of the secrets JSON: a credentials map
// keyed by credential name and a repos map keyed by repo name. Parse decodes
// into it and Template emits it, so the scaffold can never drift from what the
// parser accepts.
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

// RunFunc executes the secrets_command via the given shell and returns its
// stdout and stderr. It is injected so tests never spawn a real process.
type RunFunc func(ctx context.Context, shell, command string) (stdout, stderr []byte, err error)

// Load runs the secrets_command and parses its output into a Store. On a
// non-zero exit it surfaces neither stdout (the secret JSON) nor stderr: no
// Store — and therefore no Redactor — exists yet on this path, and a failing
// secrets provider can print secret fragments to stderr (shell tracing, a
// decrypt error echoing its input, a CLI dumping the item). Only the
// payload-free exit error is reported; the configured command is arbitrary shell
// text and may itself contain inline secrets.
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

// TemplateCred describes one credential entry the Template scaffold emits: its
// name, and whether to scaffold the s3 access_key/secret_key shorthand (when
// every repo using the credential is the s3-shorthand form) or the generic env
// map the user fills with whatever vars their backend reads.
type TemplateCred struct {
	Name string
	S3   bool
}

// Template returns a blank secrets document for the given credentials and repo
// names: a JSON shape Parse accepts, with every value left empty for the user
// to fill in and store in their secrets backend. It contains no secrets and
// reads none — it is the onboarding scaffold, generated from config before any
// secret exists, and pairs with Validate/`check` once filled in. Each
// credential scaffolds only the shape it should use (shorthand keys or env
// map), and map keys serialize alphabetically, so the output is deterministic.
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

// Validate checks that every wanted credential and repo resolves to complete
// material. Entries present in the store but absent from the wanted lists are
// returned as warnings (a shared secrets blob may legitimately list extras);
// missing or incomplete wanted entries are fatal. No secret value ever appears
// in the returned error or warnings.
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
	// Both loops above range over maps, so sort for a deterministic warning
	// order (these are logged; reordering between runs is confusing).
	sort.Strings(warnings)

	return warnings, errors.Join(errs...)
}

// validateCredEntry checks one wanted credential's shape: either the complete
// s3 shorthand pair or a non-empty env map, never a mix, with every env name
// valid and non-reserved and every value non-empty. Names appear in errors;
// values never do.
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
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Resolve returns the material for repoName using credName's backend env vars.
// credName may be empty — a repo with no credential (local/sftp backends)
// resolves to the password alone. It errors (without leaking values) if a
// named credential or the repo is missing.
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
