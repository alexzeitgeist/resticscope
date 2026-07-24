package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/BurntSushi/toml"
)

// assembleRepos validates profiles, resolves each repo's optional profile, and
// returns repos in file order with names from table keys. Profiles are fully
// resolved before Decode returns, including validation of unused profiles.
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

// validateProfile checks fields meaningful before inheritance. Partial profiles
// may omit required repository fields; URLs, credentials, and ExpectedFrequency
// are validated on merged repos.
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

// repoOrder derives repo names in document order from md.Keys. Missing names are
// appended sorted to keep the result complete and deterministic.
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
		slices.Sort(rest)
		out = append(out, rest...)
	}
	return out
}

// mergeRepoProfile fills unset repo fields from profile. Map fields merge
// key-by-key with repo values winning; empty repo scalars cannot clear inherited
// values.
func mergeRepoProfile(profile, repo Repo) Repo {
	out := profile
	// Clone map storage so merged repos cannot mutate profile maps.
	out.Env = mergeStringMaps(profile.Env, repo.Env)
	out.Options = mergeStringMaps(profile.Options, repo.Options)
	out.Labels = mergeStringMaps(profile.Labels, repo.Labels)
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
