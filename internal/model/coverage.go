package model

import (
	"strings"
	"time"
)

// Expectation is a repo's declared coverage contract, derived from the
// config's expected_* fields. It is the input to ComputeCoverage.
type Expectation struct {
	Hosts     []string
	Paths     []string
	Tags      []string
	Frequency time.Duration
}

// Coverage reports the gap between what a repo was declared to back up
// (Expectation) and what its snapshots actually show (RepoState). It is
// computed on demand and never persisted: it is cheap, and the declared
// expectations can change between runs.
type Coverage struct {
	MissingHosts []string // expected hosts with no matching snapshot
	MissingPaths []string // expected paths never seen in a snapshot
	MissingTags  []string // expected tags never seen
	Stale        bool     // newest snapshot older than Expectation.Frequency
}

// Covered reports whether every declared expectation is satisfied.
func (c Coverage) Covered() bool {
	return len(c.MissingHosts) == 0 && len(c.MissingPaths) == 0 &&
		len(c.MissingTags) == 0 && !c.Stale
}

// Summary is a one-line, plain-text description of the unmet expectations, or ""
// when coverage is fully met. Parts are ordered hosts, paths, tags, then
// staleness, so the output is stable. It is shared by the cross-repo rollup in
// `resticscope status` and the TUI coverage view; the detail view styles each
// gap separately and does not use it.
func (c Coverage) Summary() string {
	if c.Covered() {
		return ""
	}
	var parts []string
	if len(c.MissingHosts) > 0 {
		parts = append(parts, "missing hosts: "+strings.Join(c.MissingHosts, ", "))
	}
	if len(c.MissingPaths) > 0 {
		parts = append(parts, "missing paths: "+strings.Join(c.MissingPaths, ", "))
	}
	if len(c.MissingTags) > 0 {
		parts = append(parts, "missing tags: "+strings.Join(c.MissingTags, ", "))
	}
	if c.Stale {
		parts = append(parts, "stale")
	}
	return strings.Join(parts, "; ")
}

// ComputeCoverage diffs declared expectations against observed state. The
// observed hosts/paths/tags live on RepoState, captured from `restic snapshots
// --json` (from Phase 0, even though the coverage view ships later).
func ComputeCoverage(now time.Time, exp Expectation, s RepoState) Coverage {
	c := Coverage{
		MissingHosts: missing(exp.Hosts, s.Hosts),
		MissingPaths: missing(exp.Paths, s.Paths),
		MissingTags:  missing(exp.Tags, s.Tags),
	}
	if exp.Frequency > 0 {
		c.Stale = s.LastSnapshot.IsZero() || now.Sub(s.LastSnapshot) > exp.Frequency
	}
	return c
}

// missing returns the elements of want that do not appear in have. It returns
// nil (not an empty slice) when nothing is missing, so Covered and JSON output
// stay clean.
func missing(want, have []string) []string {
	if len(want) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(have))
	for _, h := range have {
		seen[h] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := seen[w]; !ok {
			out = append(out, w)
		}
	}
	return out
}
