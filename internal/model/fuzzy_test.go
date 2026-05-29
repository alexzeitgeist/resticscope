package model

import (
	"reflect"
	"sort"
	"testing"
)

// fe builds a BrowseEntry with just the fields the scorer reads (Name) plus a
// Path so deterministic tie-breaks and Path-based ranking can be asserted.
func fe(name, path string) BrowseEntry {
	return BrowseEntry{Name: name, Path: path}
}

// TestFuzzyScoreMatch covers the subsequence contract: a query matches when its
// runes appear in order (case-folded), match/non-match independent of contiguity.
func TestFuzzyScoreMatch(t *testing.T) {
	for _, tc := range []struct {
		candidate, query string
		want             bool
	}{
		{"report.txt", "rpt", true},      // scattered subsequence
		{"report.txt", "report", true},   // contiguous prefix
		{"report.txt", "txt", true},      // contiguous suffix
		{"report.txt", "rtport", false},  // out of order
		{"report.txt", "reportz", false}, // 'z' is absent
		{"abc", "abc", true},
		{"abc", "abcd", false}, // query longer than candidate
		{"abc", "", false},     // empty query never matches
		{"", "a", false},       // empty candidate, non-empty query
	} {
		_, ok := FuzzyScore(tc.candidate, tc.query)
		if ok != tc.want {
			t.Errorf("FuzzyScore(%q, %q) ok = %v, want %v", tc.candidate, tc.query, ok, tc.want)
		}
	}
}

// TestFuzzyScoreCaseInsensitiveUnicode checks case-folding and that non-ASCII
// runes both match and report rune (not byte) positions.
func TestFuzzyScoreCaseInsensitiveUnicode(t *testing.T) {
	for _, tc := range []struct {
		candidate, query string
		want             bool
		positions        []int
	}{
		{"Report.TXT", "rpt", true, []int{0, 2, 5}}, // greedy-leftmost: R@0, p@2, t@5 (folded)
		{"café", "É", true, []int{3}},               // É folds to é at rune 3
		{"日本語", "本", true, []int{1}},                // wide-rune position is a rune index
		{"café-bar", "fb", true, []int{2, 5}},       // f@2, b@5 (after the é and '-')
		{"ABC", "abc", true, []int{0, 1, 2}},
	} {
		m, ok := FuzzyScore(tc.candidate, tc.query)
		if !ok {
			t.Errorf("FuzzyScore(%q, %q) ok = false, want true", tc.candidate, tc.query)
			continue
		}
		if !reflect.DeepEqual(m.Positions, tc.positions) {
			t.Errorf("FuzzyScore(%q, %q) positions = %v, want %v", tc.candidate, tc.query, m.Positions, tc.positions)
		}
	}
}

// TestRankFuzzyOrdering asserts the relevance ordering through RankFuzzy's output
// order rather than against magic score numbers, so the constants can be retuned
// as long as the relationships hold: start beats mid, contiguous beats scattered,
// shorter beats longer, exact-case beats folded, word-boundary beats mid-word.
func TestRankFuzzyOrdering(t *testing.T) {
	for _, tc := range []struct {
		name          string
		query         string
		entries       []BrowseEntry
		first, second string // expected Name at rank 0 and rank 1
	}{
		{"start>mid", "a", []BrowseEntry{fe("xax", "/xax"), fe("axx", "/axx")}, "axx", "xax"},
		{"contiguous>scattered", "ab", []BrowseEntry{fe("axbc", "/axbc"), fe("abc", "/abc")}, "abc", "axbc"},
		{"shorter>longer", "a", []BrowseEntry{fe("aaa", "/aaa"), fe("a", "/a")}, "a", "aaa"},
		{"exactcase>folded", "A", []BrowseEntry{fe("ax", "/ax"), fe("Ax", "/Ax")}, "Ax", "ax"},
		{"boundary>midword", "b", []BrowseEntry{fe("axb", "/axb"), fe("a-b", "/a-b")}, "a-b", "axb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RankFuzzy(tc.entries, tc.query, 0)
			if len(got) != 2 {
				t.Fatalf("RankFuzzy returned %d rows, want 2", len(got))
			}
			if got[0].Name != tc.first || got[1].Name != tc.second {
				t.Errorf("order = [%q, %q], want [%q, %q]", got[0].Name, got[1].Name, tc.first, tc.second)
			}
		})
	}
}

// TestRankFuzzyDeterministicTiebreak checks the total order: when scores tie, the
// shorter name wins, and when names tie, the lexicographically smaller path wins,
// so the result never depends on input order.
func TestRankFuzzyDeterministicTiebreak(t *testing.T) {
	// Equal score (all match 'a' at the start of the same-length name) → smaller
	// path first regardless of input order.
	entries := []BrowseEntry{fe("a", "/z/a"), fe("a", "/b/a"), fe("a", "/m/a")}
	got := RankFuzzy(entries, "a", 0)
	gotPaths := make([]string, len(got))
	for i, e := range got {
		gotPaths[i] = e.Path
	}
	want := []string{"/b/a", "/m/a", "/z/a"}
	if !reflect.DeepEqual(gotPaths, want) {
		t.Errorf("paths = %v, want %v", gotPaths, want)
	}
}

// TestRankFuzzyLimit caps the result to limit while limit<=0 returns every match.
func TestRankFuzzyLimit(t *testing.T) {
	entries := []BrowseEntry{fe("a1", "/a1"), fe("a2", "/a2"), fe("a3", "/a3"), fe("a4", "/a4")}
	if got := RankFuzzy(entries, "a", 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d rows, want 2", len(got))
	}
	if got := RankFuzzy(entries, "a", 0); len(got) != 4 {
		t.Errorf("limit 0 returned %d rows, want 4 (all)", len(got))
	}
	if got := RankFuzzy(entries, "a", 10); len(got) != 4 {
		t.Errorf("limit 10 returned %d rows, want 4 (all)", len(got))
	}
}

// TestRankFuzzyEmptyQuery is the search contract: blank or whitespace-only queries
// rank nothing (the store skips the scan; FuzzyScore rejects a truly empty query).
func TestRankFuzzyEmptyQuery(t *testing.T) {
	entries := []BrowseEntry{fe("a", "/a"), fe("b", "/b")}
	for _, q := range []string{"", "   ", "\t"} {
		if got := RankFuzzy(entries, q, 0); got != nil {
			t.Errorf("RankFuzzy(%q) = %v, want nil", q, got)
		}
	}
	if _, ok := FuzzyScore("anything", ""); ok {
		t.Errorf("FuzzyScore with empty query ok = true, want false")
	}
}

// TestBetterFuzzy pins the comparator directly so the store's bounded top-N and
// RankFuzzy share one total order: score desc, then name-rune-length asc, then
// path asc.
func TestBetterFuzzy(t *testing.T) {
	hi := FuzzyRank{Entry: fe("y", "/y"), Match: FuzzyMatch{Score: 10}}
	lo := FuzzyRank{Entry: fe("x", "/x"), Match: FuzzyMatch{Score: 5}}
	if !BetterFuzzy(hi, lo) || BetterFuzzy(lo, hi) {
		t.Errorf("higher score should rank first")
	}

	// Equal score: shorter name wins even when its path sorts later.
	short := FuzzyRank{Entry: fe("ab", "/z"), Match: FuzzyMatch{Score: 5}}
	long := FuzzyRank{Entry: fe("abc", "/a"), Match: FuzzyMatch{Score: 5}}
	if !BetterFuzzy(short, long) || BetterFuzzy(long, short) {
		t.Errorf("equal score: shorter name should rank first")
	}

	// Equal score and name length: smaller path wins.
	pa := FuzzyRank{Entry: fe("a", "/a"), Match: FuzzyMatch{Score: 5}}
	pb := FuzzyRank{Entry: fe("a", "/b"), Match: FuzzyMatch{Score: 5}}
	if !BetterFuzzy(pa, pb) || BetterFuzzy(pb, pa) {
		t.Errorf("equal score and name length: smaller path should rank first")
	}
}

// TestRankFuzzyMatchesBetterFuzzy is the parity check: RankFuzzy's output equals
// sorting the same matches by BetterFuzzy, so the store accumulator (which uses
// BetterFuzzy) and RankFuzzy cannot drift.
func TestRankFuzzyMatchesBetterFuzzy(t *testing.T) {
	entries := []BrowseEntry{
		fe("report.txt", "/a/report.txt"),
		fe("rpt", "/b/rpt"),
		fe("xreportx", "/c/xreportx"),
		fe("Report", "/d/Report"),
		fe("nope", "/e/nope"),
	}
	const query = "rpt"

	got := RankFuzzy(entries, query, 0)

	// Build the expected order independently via BetterFuzzy.
	var ranks []FuzzyRank
	for _, e := range entries {
		if m, ok := FuzzyScore(e.Name, query); ok {
			ranks = append(ranks, FuzzyRank{Entry: e, Match: m})
		}
	}
	sort.SliceStable(ranks, func(i, j int) bool { return BetterFuzzy(ranks[i], ranks[j]) })

	if len(got) != len(ranks) {
		t.Fatalf("RankFuzzy returned %d rows, BetterFuzzy ordering has %d", len(got), len(ranks))
	}
	for i := range got {
		if got[i].Path != ranks[i].Entry.Path {
			t.Errorf("rank %d: RankFuzzy=%q, BetterFuzzy=%q", i, got[i].Path, ranks[i].Entry.Path)
		}
	}
}
