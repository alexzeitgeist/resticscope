package model

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Filename search uses a shared case-folded subsequence scorer so browsedb and
// the TUI apply the same ordering.

// Coarse weights favor starts, contiguous runs, exact case, and shorter names.
const (
	fuzzyMatchBonus      = 16 // each matched rune
	fuzzyContiguityBonus = 8  // matched rune immediately follows the previous match
	fuzzyStartBonus      = 16 // match at the very start of the name
	fuzzyBoundaryBonus   = 8  // match right after a word separator (/ . - _ space)
	fuzzyExactCaseBonus  = 2  // matched rune has the same case as the query rune
	fuzzyLengthPenalty   = 1  // subtracted per candidate rune (shorter wins ties)
)

// BrowseSearchResult contains ranked, capped rows and the number of matches
// before the cap.
type BrowseSearchResult struct {
	Rows  []BrowseEntry
	Total int
}

// FuzzyMatch contains a relevance score and ascending matched-rune positions.
type FuzzyMatch struct {
	Score     int
	Positions []int
}

// FuzzyRank pairs a browser entry with its fuzzy match for shared ordering.
type FuzzyRank struct {
	Entry BrowseEntry
	Match FuzzyMatch
}

// FuzzyScore reports whether query is a case-folded rune subsequence of
// candidate and returns its score and matched positions. Empty queries do not
// match; spaces in nonblank queries are literal. Per-rune unicode.ToLower
// folding matches the browser index's case-folded SQL prefilter.
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

// isWordBoundary reports whether r separates words in a filename.
func isWordBoundary(r rune) bool {
	switch r {
	case '/', '.', '-', '_', ' ':
		return true
	}
	return false
}

// BetterFuzzy reports whether a ranks before b by score, rune length, then path.
// The tie-breakers provide deterministic ordering across ranking implementations.
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

// RankFuzzy returns matching entries in fuzzy order, capped when limit is
// positive. A blank query returns nil.
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
