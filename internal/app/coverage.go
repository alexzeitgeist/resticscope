package app

import "resticscope/internal/model"

// CoverageGap pairs a repo with its computed coverage, recorded only when that
// coverage has at least one unmet expectation. It is the per-repo detail behind
// a rollup.
type CoverageGap struct {
	Repo     string
	Coverage model.Coverage
}

// CoverageRollup is the cross-repo aggregate of declared-vs-observed coverage:
// how many repos meet every expectation, and the specific gaps for those that
// do not. It is computed on demand from already-evaluated status rows (each of
// which carries its repo's Coverage), so it makes no S3 or restic calls.
type CoverageRollup struct {
	Total   int           // repos considered
	Covered int           // repos meeting every declared expectation
	Gaps    []CoverageGap // repos with unmet expectations, in input (config) order
}

// FullyCovered reports whether every considered repo met its expectations.
func (r CoverageRollup) FullyCovered() bool {
	return len(r.Gaps) == 0
}

// Rollup aggregates the per-repo coverage carried on a set of status rows into a
// single cross-repo verdict. Rows are considered in the order given (config
// order), and that order is preserved in Gaps.
func Rollup(rows []RepoStatus) CoverageRollup {
	r := CoverageRollup{Total: len(rows)}
	for _, row := range rows {
		if row.Coverage.Covered() {
			r.Covered++
			continue
		}
		r.Gaps = append(r.Gaps, CoverageGap{Repo: row.Name, Coverage: row.Coverage})
	}
	return r
}
