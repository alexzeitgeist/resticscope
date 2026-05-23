// Package secrets resolves S3 keys and restic passwords at runtime by running
// the user's secrets_command and parsing its JSON output.
//
// Secrets live only in memory for the lifetime of the process. They are never
// written to the cache, logs, or error strings. The Redactor exists so that
// any text that might contain a secret (restic stderr, log lines) can be
// scrubbed before it leaves the process. This is the highest-risk package in
// resticscope; treat it accordingly.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Material is the resolved secret bundle for a single repo: the S3 key pair
// from its credential plus the repo's own restic password.
type Material struct {
	AccessKey      string
	SecretKey      string
	ResticPassword string
}

type credEntry struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type repoEntry struct {
	ResticPassword string `json:"restic_password"`
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
// payload-free exit error is reported.
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
	var raw struct {
		Credentials map[string]credEntry `json:"credentials"`
		Repos       map[string]repoEntry `json:"repos"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// Never include data in the error: it is the secret payload.
		return nil, fmt.Errorf("parse secrets JSON: %w", err)
	}
	return &Store{credentials: raw.Credentials, repos: raw.Repos}, nil
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
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("secrets: credential %q missing from credentials map", name))
		case c.AccessKey == "" || c.SecretKey == "":
			errs = append(errs, fmt.Errorf("secrets: credential %q missing access_key or secret_key", name))
		}
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

	return warnings, errors.Join(errs...)
}

// Resolve returns the material for repoName using credName's S3 keys. It errors
// (without leaking values) if either is missing.
func (s *Store) Resolve(repoName, credName string) (Material, error) {
	c, ok := s.credentials[credName]
	if !ok {
		return Material{}, fmt.Errorf("secrets: no credential %q", credName)
	}
	r, ok := s.repos[repoName]
	if !ok {
		return Material{}, fmt.Errorf("secrets: no repo %q", repoName)
	}
	return Material{
		AccessKey:      c.AccessKey,
		SecretKey:      c.SecretKey,
		ResticPassword: r.ResticPassword,
	}, nil
}
