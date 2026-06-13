package config

import (
	"errors"
	"fmt"
	"maps"
	"sort"

	"github.com/BurntSushi/toml"
)

// A profile is a named set of repo fields, declared as `[profiles.<name>]`,
// that repos opt into with `profile = "<name>"`. It is the single mechanism for
// sharing common credential/endpoint/frequency/label settings across repos.
// Profiles are decoded as ordinary Repo values and resolved here, so once Decode
// returns, runtime code only ever sees fully merged repos — no profile concept
// survives into the rest of the program.

// assembleRepos flattens the decoded `[repos.<name>]` tables into a file-ordered
// slice, sets each repo's Name from its table key, and resolves profile
// inheritance. Every profile is checked first (see validateProfile) so a typo in
// one fails at config load even before any repo points at it.
func assembleRepos(byName, profiles map[string]Repo, md toml.MetaData) ([]Repo, error) {
	var perrs []error
	for _, name := range sortedKeys(profiles) {
		perrs = append(perrs, validateProfile(name, profiles[name])...)
	}
	if err := errors.Join(perrs...); err != nil {
		return nil, err
	}

	out := make([]Repo, 0, len(byName))
	for _, name := range repoOrder(md, byName) {
		r := byName[name]
		if r.Name != "" {
			return nil, fmt.Errorf("repos.%s: name is not allowed; the repo name is the table key", name)
		}
		r.Name = name
		if r.Profile != "" {
			p, ok := profiles[r.Profile]
			if !ok {
				return nil, fmt.Errorf("repo %q: profile %q does not match any [profiles.<name>] table", name, r.Profile)
			}
			r = mergeRepoProfile(p, r)
		}
		out = append(out, r)
	}
	return out, nil
}

// validateProfile checks the fields of a `[profiles.<name>]` table that are
// well-formed in isolation, so a typo is caught at config load even if no repo
// uses the profile yet — without forcing a profile to be a complete repo. It
// deliberately does NOT check:
//   - endpoint/bucket presence — partial profiles (shared fields only) are the
//     whole point;
//   - url — its leading ~ is only expanded during Normalize, after profiles are
//     already merged, so a profile url is validated on the merged repo;
//   - credential — it is never config-validated (used or not); it is just a name
//     resolved against the secrets document when a command loads secrets;
//   - expected_frequency — a consuming repo re-validates it (> 0) on the merged
//     repo, and an unused profile's value never takes effect.
func validateProfile(name string, p Repo) []error {
	label := "profiles." + name
	var errs []error
	switch {
	case p.Name != "":
		errs = append(errs, fmt.Errorf("%s: name is not allowed; a profile is named by its table key", label))
	case p.Profile != "":
		errs = append(errs, fmt.Errorf("%s: profile is not allowed inside a profile", label))
	}
	if p.BucketLookup != "" && !validBucketLookup[p.BucketLookup] {
		errs = append(errs, fmt.Errorf("%s: bucket_lookup must be auto|dns|path, got %q", label, p.BucketLookup))
	}
	errs = append(errs, validateEnvOptions(label, p.Env, p.Options)...)
	return errs
}

// repoOrder returns the repo names in the order their tables appear in the file.
// TOML decodes `[repos.<name>]` into an unordered Go map, so file order is
// recovered from the decode metadata (md.Keys() lists keys in document order).
// Any name the metadata fails to surface — which should not happen — is appended
// in sorted order so the result is always complete and deterministic.
func repoOrder(md toml.MetaData, byName map[string]Repo) []string {
	seen := make(map[string]bool, len(byName))
	out := make([]string, 0, len(byName))
	for _, key := range md.Keys() {
		if len(key) < 2 || key[0] != "repos" {
			continue
		}
		name := key[1]
		if seen[name] {
			continue
		}
		if _, ok := byName[name]; !ok {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) < len(byName) {
		rest := make([]string, 0, len(byName)-len(out))
		for name := range byName {
			if !seen[name] {
				rest = append(rest, name)
			}
		}
		sort.Strings(rest)
		out = append(out, rest...)
	}
	return out
}

// mergeRepoProfile returns the repo with every field it leaves unset filled in
// from the profile. Scalar fields take the repo's value when it is non-zero;
// labels, env, and options are merged key-by-key with the repo winning on
// conflicts. A field the profile sets cannot be cleared back to empty by a repo:
// a repo overrides with a different value, not with absence.
func mergeRepoProfile(profile, repo Repo) Repo {
	out := profile
	if repo.Credential != "" {
		out.Credential = repo.Credential
	}
	if repo.URL != "" {
		out.URL = repo.URL
	}
	if repo.Endpoint != "" {
		out.Endpoint = repo.Endpoint
	}
	if repo.Region != "" {
		out.Region = repo.Region
	}
	if repo.Bucket != "" {
		out.Bucket = repo.Bucket
	}
	if repo.Path != "" {
		out.Path = repo.Path
	}
	if repo.BucketLookup != "" {
		out.BucketLookup = repo.BucketLookup
	}
	if repo.ExpectedFrequency != 0 {
		out.ExpectedFrequency = repo.ExpectedFrequency
	}
	out.Env = mergeStringMaps(profile.Env, repo.Env)
	out.Options = mergeStringMaps(profile.Options, repo.Options)
	out.Labels = mergeStringMaps(profile.Labels, repo.Labels)
	out.Name = repo.Name
	out.Profile = repo.Profile
	return out
}

// mergeStringMaps overlays override onto base, returning nil when both are empty
// so an unset map stays nil rather than becoming an empty non-nil map.
func mergeStringMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	maps.Copy(out, base)
	maps.Copy(out, override)
	return out
}
