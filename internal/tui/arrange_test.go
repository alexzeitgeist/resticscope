package tui

import (
	"reflect"
	"testing"
	"time"

	"resticscope/internal/model"
)

// arrange_test.go covers the pure browse sorter (sortedBrowseRows/browseLess). The
// tests build []model.BrowseEntry inline and assert the display order by Path, plus
// the two structural invariants the wiring relies on: name mode aliases the
// canonical slice (no copy) while size/modified return a distinct backing array and
// never mutate the input.

// browseRowPaths is the Path of each row in order, the shape the order assertions
// compare on (paths are unique within a directory listing, so they identify rows).
func browseRowPaths(rows []model.BrowseEntry) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].Path
	}
	return out
}

// canonical is the dirs-first, case-insensitive-name order browsedb.ListDir
// returns, reused as the input for the sorter tests. The files deliberately have a
// name order (a→b→c) that disagrees with both their size order and their mtime
// order, so each mode produces a provably distinct permutation.
func sorterFixture() []model.BrowseEntry {
	mid := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	return []model.BrowseEntry{
		{Path: "/d1", Name: "d1", IsDir: true},
		{Path: "/d2", Name: "d2", IsDir: true},
		{Path: "/a.txt", Name: "a.txt", Size: 100, ModTime: mid},
		{Path: "/b.txt", Name: "b.txt", Size: 300, ModTime: old},
		{Path: "/c.txt", Name: "c.txt", Size: 200, ModTime: newt},
	}
}

func TestSortedBrowseRowsOrders(t *testing.T) {
	tests := []struct {
		name string
		mode browseSortMode
		want []string
	}{
		{
			// name mode is the canonical order verbatim.
			name: "name is canonical",
			mode: browseSortName,
			want: []string{"/d1", "/d2", "/a.txt", "/b.txt", "/c.txt"},
		},
		{
			// dirs stay first (their zero size does not sink them below the files),
			// then files largest-first: b(300) c(200) a(100).
			name: "size largest first, dirs first",
			mode: browseSortSize,
			want: []string{"/d1", "/d2", "/b.txt", "/c.txt", "/a.txt"},
		},
		{
			// dirs first again (their zero mtime does not sink them), then files
			// newest-first: c(May) a(Mar) b(Jan).
			name: "modified newest first, dirs first",
			mode: browseSortModified,
			want: []string{"/d1", "/d2", "/c.txt", "/a.txt", "/b.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := browseRowPaths(sortedBrowseRows(sorterFixture(), tt.mode))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sortedBrowseRows(%s) = %v, want %v", tt.mode.label(), got, tt.want)
			}
		})
	}
}

// Equal sort keys fall through to the case-insensitive name tie-break, so cycling
// is reproducible (the order is total). Two files share a size and two share an
// mtime; each pair must come out in name order.
func TestSortedBrowseRowsTieBreakByName(t *testing.T) {
	ts := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	in := []model.BrowseEntry{
		{Path: "/Beta.txt", Name: "Beta.txt", Size: 100, ModTime: ts},
		{Path: "/alpha.txt", Name: "alpha.txt", Size: 100, ModTime: ts},
	}
	// size: equal sizes → case-insensitive name (alpha < Beta).
	if got := browseRowPaths(sortedBrowseRows(in, browseSortSize)); !reflect.DeepEqual(got, []string{"/alpha.txt", "/Beta.txt"}) {
		t.Errorf("equal-size tie-break = %v, want [/alpha.txt /Beta.txt]", got)
	}
	// modified: equal mtimes → same case-insensitive name tie-break.
	if got := browseRowPaths(sortedBrowseRows(in, browseSortModified)); !reflect.DeepEqual(got, []string{"/alpha.txt", "/Beta.txt"}) {
		t.Errorf("equal-mtime tie-break = %v, want [/alpha.txt /Beta.txt]", got)
	}
}

// An unknown (zero) ModTime sorts last among files in modified mode — after every
// known time — then the zero-time group is name-ordered among itself.
func TestSortedBrowseRowsModifiedZeroTimeLast(t *testing.T) {
	known := time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC)
	in := []model.BrowseEntry{
		{Path: "/zzz-unknown.txt", Name: "zzz-unknown.txt"}, // zero mtime, but name sorts first
		{Path: "/known.txt", Name: "known.txt", ModTime: known},
		{Path: "/aaa-unknown.txt", Name: "aaa-unknown.txt"}, // zero mtime
	}
	got := browseRowPaths(sortedBrowseRows(in, browseSortModified))
	want := []string{"/known.txt", "/aaa-unknown.txt", "/zzz-unknown.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("modified with zero mtimes = %v, want %v", got, want)
	}
}

// The order is deterministic regardless of input order: a scrambled slice and the
// canonical slice yield identical output, because the comparator is total (the
// unique Path is the final tie-break).
func TestSortedBrowseRowsDeterministicAcrossInputOrder(t *testing.T) {
	canonical := sorterFixture()
	scrambled := []model.BrowseEntry{canonical[3], canonical[0], canonical[4], canonical[2], canonical[1]}
	for _, mode := range []browseSortMode{browseSortSize, browseSortModified} {
		fromCanonical := browseRowPaths(sortedBrowseRows(canonical, mode))
		fromScrambled := browseRowPaths(sortedBrowseRows(scrambled, mode))
		if !reflect.DeepEqual(fromCanonical, fromScrambled) {
			t.Errorf("%s not deterministic: canonical=%v scrambled=%v", mode.label(), fromCanonical, fromScrambled)
		}
	}
}

// name mode returns the input slice itself (aliases the canonical cached slice, no
// copy), while size/modified return a distinct backing array and leave the input
// untouched — the invariant that keeps browseCache canonical.
func TestSortedBrowseRowsBackingArrayAndNoMutation(t *testing.T) {
	canonical := sorterFixture()

	// name aliases the input: same backing array, no copy.
	name := sortedBrowseRows(canonical, browseSortName)
	if len(name) == 0 || &name[0] != &canonical[0] {
		t.Error("name mode must return the input slice unchanged (aliasing the canonical cache)")
	}

	for _, mode := range []browseSortMode{browseSortSize, browseSortModified} {
		before := browseRowPaths(canonical)
		got := sortedBrowseRows(canonical, mode)
		if &got[0] == &canonical[0] {
			t.Errorf("%s must sort a copy, not alias the canonical slice", mode.label())
		}
		if after := browseRowPaths(canonical); !reflect.DeepEqual(before, after) {
			t.Errorf("%s mutated the input: before=%v after=%v", mode.label(), before, after)
		}
	}
}

func TestBrowseSortModeLabel(t *testing.T) {
	cases := map[browseSortMode]string{
		browseSortName:     "name",
		browseSortSize:     "size",
		browseSortModified: "modified",
	}
	for mode, want := range cases {
		if got := mode.label(); got != want {
			t.Errorf("browseSortMode(%d).label() = %q, want %q", mode, got, want)
		}
	}
}
