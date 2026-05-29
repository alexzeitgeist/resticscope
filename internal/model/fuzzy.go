package model

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fuzzy.go is the pure, credential-free scorer behind the snapshot browser's
// global filename search. It lives in the leaf model package so both the store
// (browsedb.Search, which ranks its SQL-prefiltered rows) and the TUI can share
// one ordering and never drift. It holds filenames only as in/out arguments —
// nothing here persists, logs, or returns a path on its own.
//
// The match is fzf-style: a query matches a candidate when the query's runes
// appear in order (a case-folded subsequence), not necessarily adjacent. Among
// matches, a small bonus model rewards the matches that read as "more relevant"
// to a human — contiguous runs, hits at the start of a name or after a word
// separator, and exact-case hits — while a per-rune length penalty lets a
// shorter name win an otherwise-equal contest.

// Fuzzy scoring weights. They are deliberately coarse: the goal is a stable,
// explainable ordering (start-of-name beats mid-name, contiguous beats
// scattered, exact-case and shorter break near-ties), not a finely tuned metric.
// The length penalty is subtracted once per candidate rune so a shorter name
// outscores a longer one that matched the same way.
const (
	fuzzyMatchBonus      = 16 // each matched rune
	fuzzyContiguityBonus = 8  // matched rune immediately follows the previous match
	fuzzyStartBonus      = 16 // match at the very start of the name
	fuzzyBoundaryBonus   = 8  // match right after a word separator (/ . - _ space)
	fuzzyExactCaseBonus  = 2  // matched rune has the same case as the query rune
	fuzzyLengthPenalty   = 1  // subtracted per candidate rune (shorter wins ties)
)

// BrowseSearchResult is the outcome of a global filename search: the best Rows
// (already ranked and capped to the caller's limit) plus Total, the count of
// every node that matched before the cap. Total > len(Rows) tells the UI to show
// "showing N of Total" so a truncated result never masquerades as complete.
type BrowseSearchResult struct {
	Rows  []BrowseEntry
	Total int
}

// FuzzyMatch is the result of scoring one candidate against a query: its
// relevance Score and the matched rune Positions (indices into the candidate's
// runes, ascending). Positions support a later match highlight; v1 may ignore
// them, but they are part of the scorer's contract and are unit-tested.
type FuzzyMatch struct {
	Score     int
	Positions []int
}

// FuzzyRank pairs a matched entry with its score. It is the unit the shared
// comparator orders, so the store's bounded top-N accumulator and the pure
// RankFuzzy used in tests apply byte-identical ordering.
type FuzzyRank struct {
	Entry BrowseEntry
	Match FuzzyMatch
}

// FuzzyScore reports whether query is a case-folded subsequence of candidate
// and, when it is, returns the match's score and matched rune positions. ok is
// false when any query rune cannot be consumed in order. An empty query never
// matches: there is nothing to rank and an "everything matches" answer would be
// useless for search (the whitespace-only case is screened earlier, by RankFuzzy
// and browsedb.Search, so a query that is only spaces never reaches scoring; a
// query with non-space runes treats its spaces as literal subsequence chars).
//
// Matching folds case per rune with unicode.ToLower, the same fold strings.ToLower
// applies when building name_ci, so the in-Go score agrees with the SQL LIKE
// prefilter and cannot reject a row the prefilter accepted.
func FuzzyScore(candidate, query string) (FuzzyMatch, bool) {
	if len(query) == 0 {
		return FuzzyMatch{}, false
	}
	cand := []rune(candidate)

	positions := make([]int, 0, utf8.RuneCountInString(query))
	score := 0
	prev := -2 // < -1 so the first match is never scored as contiguous
	ci := 0    // next unconsumed candidate rune index
	for _, qr := range query {
		ql := unicode.ToLower(qr)
		matched := false
		for ci < len(cand) {
			cr := cand[ci]
			if unicode.ToLower(cr) == ql {
				score += fuzzyMatchBonus
				switch {
				case ci == 0:
					score += fuzzyStartBonus
				case ci == prev+1:
					score += fuzzyContiguityBonus
				case isWordBoundary(cand[ci-1]):
					score += fuzzyBoundaryBonus
				}
				if cr == qr {
					score += fuzzyExactCaseBonus
				}
				positions = append(positions, ci)
				prev = ci
				ci++
				matched = true
				break
			}
			ci++
		}
		if !matched {
			return FuzzyMatch{}, false
		}
	}
	score -= fuzzyLengthPenalty * len(cand)
	return FuzzyMatch{Score: score, Positions: positions}, true
}

// isWordBoundary reports whether r is a separator that makes the following rune
// read as the start of a new word — the path, extension, and word delimiters a
// filename uses. A match right after one of these earns the boundary bonus.
func isWordBoundary(r rune) bool {
	switch r {
	case '/', '.', '-', '_', ' ':
		return true
	}
	return false
}

// BetterFuzzy reports whether a should rank before b. The primary key is score
// (higher first); ties break by name length in runes (shorter first) then by
// Path (lexicographically smaller first). The two tie-breakers make the order
// total and deterministic, so equal-scoring matches never shuffle between runs
// or between the store's accumulator and a pure RankFuzzy. It is exported so
// browsedb.Search's bounded top-N uses the very same predicate.
func BetterFuzzy(a, b FuzzyRank) bool {
	if a.Match.Score != b.Match.Score {
		return a.Match.Score > b.Match.Score
	}
	an := utf8.RuneCountInString(a.Entry.Name)
	bn := utf8.RuneCountInString(b.Entry.Name)
	if an != bn {
		return an < bn
	}
	return a.Entry.Path < b.Entry.Path
}

// RankFuzzy scores every entry against query, drops the non-matches, orders the
// rest by BetterFuzzy, and returns at most limit entries (all matches when
// limit <= 0). A blank or whitespace-only query returns nil — search has nothing
// to rank. This is the pure reference ranking; browsedb.Search reproduces the
// same ordering incrementally over its SQL-prefiltered rows.
func RankFuzzy(entries []BrowseEntry, query string, limit int) []BrowseEntry {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	ranks := make([]FuzzyRank, 0, len(entries))
	for _, e := range entries {
		if match, ok := FuzzyScore(e.Name, query); ok {
			ranks = append(ranks, FuzzyRank{Entry: e, Match: match})
		}
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		return BetterFuzzy(ranks[i], ranks[j])
	})
	if limit > 0 && len(ranks) > limit {
		ranks = ranks[:limit]
	}
	out := make([]BrowseEntry, len(ranks))
	for i, r := range ranks {
		out[i] = r.Entry
	}
	return out
}
