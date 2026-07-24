package browsedb

import (
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// entriesFromNodes projects index nodes to the (Name, Path) pairs the pure model
// ranker needs, so a DB search result can be checked against model.RankFuzzy over
// the same set — pinning DB-search ranking to the pure scorer.
func entriesFromNodes(nodes []model.BrowseNode) []model.BrowseEntry {
	out := make([]model.BrowseEntry, 0, len(nodes))
	for _, n := range nodes {
		p := model.CleanBrowsePath(n.Path)
		out = append(out, model.BrowseEntry{Name: model.BrowseName(n.Name, p), Path: p})
	}
	return out
}

// TestLikeSubsequence pins the prefilter pattern builder: each rune wrapped in
// '%', with the LIKE specials (% _ \) backslash-escaped so they match literally
// under ESCAPE '\'.
func TestLikeSubsequence(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"abc", "%a%b%c%"},
		{"", "%"},
		{"a%b", `%a%\%%b%`},
		{"a_b", `%a%\_%b%`},
		{`a\b`, `%a%\\%b%`},
	} {
		if got := likeSubsequence(tc.in); got != tc.want {
			t.Errorf("likeSubsequence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSearchSubsequenceAcrossDirs confirms a global subsequence query matches
// nodes anywhere in the snapshot and reconstructs each one's full path from its
// own parent directory (not a single requested parent).
func TestSearchSubsequenceAcrossDirs(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/etc", IsDir: true},
		{Path: "/home", IsDir: true},
		{Path: "/home/alex", IsDir: true},
		{Path: "/etc/report.txt"},
		{Path: "/home/alex/receipt.pdf"},
		{Path: "/home/alex/photo.jpg"},
	})

	// "rpt" is a subsequence of both report.txt and receipt.pdf, neither contiguous,
	// and they live in different directories.
	res, err := db.Search(ctx, repo, snap, "rpt", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
	want := []string{"/etc/report.txt", "/home/alex/receipt.pdf"}
	got := paths(res.Rows)
	// Order is by score, but both are valid full paths from distinct dirs; assert the
	// set regardless of order.
	if !sameSet(got, want) {
		t.Errorf("paths = %v, want set %v", got, want)
	}
}

// TestSearchFullPathMatchesListDir proves a searched row reconstructs the exact
// same Path (and metadata) ListDir produces for the same node.
func TestSearchFullPathMatchesListDir(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	zone := time.FixedZone("X", 2*3600)
	mt := time.Date(2021, 3, 4, 5, 6, 7, 0, zone)
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/etc", IsDir: true},
		{Path: "/etc/passwd", Type: "file", Size: 99, ModTime: mt, OwnerKnown: true, UID: 1000, GID: 1000, Permissions: "-rw-r--r--"},
	})

	listed, _ := db.ListDir(ctx, repo, snap, "/etc")
	if len(listed) != 1 {
		t.Fatalf("ListDir(/etc) returned %d rows, want 1", len(listed))
	}
	res, err := db.Search(ctx, repo, snap, "passwd", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("Search returned %d rows, want 1", len(res.Rows))
	}
	if res.Rows[0] != listed[0] {
		t.Errorf("search row = %+v, want identical to listed %+v", res.Rows[0], listed[0])
	}
}

// TestSearchLikeSpecialsLiteral proves a query containing LIKE wildcards matches
// them literally: "a%b" matches "a%b" but not "axb"; "a_b" matches "a_b" only.
func TestSearchLikeSpecialsLiteral(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/a%b"},
		{Path: "/a_b"},
		{Path: "/axb"},
		{Path: "/ayb"},
	})

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"a%b", []string{"/a%b"}},
		{"a_b", []string{"/a_b"}},
	} {
		res, err := db.Search(ctx, repo, snap, tc.query, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", tc.query, err)
		}
		if !sameSet(paths(res.Rows), tc.want) || res.Total != len(tc.want) {
			t.Errorf("Search(%q) = %v (total %d), want %v", tc.query, paths(res.Rows), res.Total, tc.want)
		}
	}
}

// TestSearchLiteralSpaces proves spaces in a non-empty query are literal
// subsequence characters: " re" matches a name with a leading space but not one
// without it.
func TestSearchLiteralSpaces(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/ report", Name: " report"},
		{Path: "/report", Name: "report"},
	})

	res, err := db.Search(ctx, repo, snap, " re", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"/ report"}; !sameSet(paths(res.Rows), want) || res.Total != 1 {
		t.Errorf("Search(%q) = %v (total %d), want %v", " re", paths(res.Rows), res.Total, want)
	}
}

// TestSearchSidGatedOnIndexed proves search only sees committed snapshots and
// never crosses snapshot boundaries: a rolled-back index is invisible, and one
// snapshot's query never returns another's nodes.
func TestSearchSidGatedOnIndexed(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()

	// Rolled-back index → no committed marker → invisible to search.
	itx, _ := db.BeginIndex(ctx, "repo", "pending")
	if err := itx.Add(ctx, model.BrowseNode{Path: "/secretfile"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := itx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res, err := db.Search(ctx, "repo", "pending", "secret", 0); err != nil || len(res.Rows) != 0 || res.Total != 0 {
		t.Errorf("search of rolled-back snapshot = %v (total %d), %v; want empty", paths(res.Rows), res.Total, err)
	}

	// Two committed snapshots with distinct nodes: each query stays in its own sid.
	mustIndex(t, db, "repo", "A", []model.BrowseNode{{Path: "/onlyA"}})
	mustIndex(t, db, "repo", "B", []model.BrowseNode{{Path: "/onlyB"}})
	res, err := db.Search(ctx, "repo", "A", "only", 0)
	if err != nil {
		t.Fatalf("Search(A): %v", err)
	}
	if want := []string{"/onlyA"}; !sameSet(paths(res.Rows), want) {
		t.Errorf("Search(A, %q) = %v, want %v (no cross-snapshot leak)", "only", paths(res.Rows), want)
	}
}

// TestSearchResultLimitAndTotal proves the cap returns the exact ranked prefix
// (not an arbitrary scan prefix) while Total counts every match, and that the
// DB ranking is identical to the pure model.RankFuzzy over the same set.
func TestSearchResultLimitAndTotal(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	nodes := []model.BrowseNode{
		{Path: "/apple"},
		{Path: "/data/aardvark"},
		{Path: "/data/banana"},
		{Path: "/x/abacus"},
		{Path: "/x/y/cassava"},
		{Path: "/area"},
		{Path: "/dirs", IsDir: true},
		{Path: "/dirs/saga"},
	}
	mustIndex(t, db, repo, snap, nodes)
	const query = "a"

	full, err := db.Search(ctx, repo, snap, query, 0)
	if err != nil {
		t.Fatalf("Search(full): %v", err)
	}
	// Independent reference ranking from the pure model over the same names.
	want := paths(model.RankFuzzy(entriesFromNodes(nodes), query, 0))
	if !eqStrings(paths(full.Rows), want) {
		t.Fatalf("full ranking = %v, want %v", paths(full.Rows), want)
	}
	if full.Total != len(want) {
		t.Errorf("Total = %d, want %d", full.Total, len(want))
	}

	// A small cap returns exactly the top-N prefix of the full ranking, with Total
	// unchanged.
	const limit = 3
	capped, err := db.Search(ctx, repo, snap, query, limit)
	if err != nil {
		t.Fatalf("Search(capped): %v", err)
	}
	if len(capped.Rows) != limit {
		t.Fatalf("capped rows = %d, want %d", len(capped.Rows), limit)
	}
	if !eqStrings(paths(capped.Rows), want[:limit]) {
		t.Errorf("capped ranking = %v, want %v", paths(capped.Rows), want[:limit])
	}
	if capped.Total != full.Total {
		t.Errorf("capped Total = %d, want %d", capped.Total, full.Total)
	}
}

// TestSearchEmptyQuery is the no-scan contract: blank or whitespace-only queries
// return a zero result with no error.
func TestSearchEmptyQuery(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/file"}})
	for _, q := range []string{"", "   ", "\t"} {
		res, err := db.Search(ctx, repo, snap, q, 0)
		if err != nil {
			t.Errorf("Search(%q) err = %v, want nil", q, err)
		}
		if len(res.Rows) != 0 || res.Total != 0 {
			t.Errorf("Search(%q) = %v (total %d), want empty", q, paths(res.Rows), res.Total)
		}
	}
}

// TestSearchMetadataRoundTrip proves a matched row carries the same node metadata
// the listing path does (mtime/perms/owner/is_dir/link_target/size).
func TestSearchMetadataRoundTrip(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	zone := time.FixedZone("X", 2*3600)
	mt := time.Date(2021, 3, 4, 5, 6, 7, 0, zone)
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/linkmatch", Type: "symlink", LinkTarget: "/target", OwnerKnown: true, UID: 5, GID: 6, ModTime: mt, Permissions: "lrwxrwxrwx"},
	})
	res, err := db.Search(ctx, repo, snap, "link", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	e := res.Rows[0]
	if e.Path != "/linkmatch" || e.Type != "symlink" || e.LinkTarget != "/target" || e.IsDir {
		t.Errorf("type/link/isdir wrong: %+v", e)
	}
	if !e.OwnerKnown || e.UID != 5 || e.GID != 6 || e.Permissions != "lrwxrwxrwx" {
		t.Errorf("owner/perms wrong: %+v", e)
	}
	if got := e.ModTime.Format("2006-01-02 15:04"); got != "2021-03-04 05:06" {
		t.Errorf("mtime wall clock = %q, want 2021-03-04 05:06", got)
	}
}

// TestSearchCaseFoldParity proves matching folds case the same way name_ci was
// built: a query in any case matches, and the lowercase pattern agrees with the
// stored fold.
func TestSearchCaseFoldParity(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/Report.TXT"}})
	for _, q := range []string{"rpt", "RPT", "Rpt", "report", "REPORT"} {
		res, err := db.Search(ctx, repo, snap, q, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if want := []string{"/Report.TXT"}; !sameSet(paths(res.Rows), want) {
			t.Errorf("Search(%q) = %v, want %v", q, paths(res.Rows), want)
		}
	}
}

// TestSearchErrorPathFree proves a search failure never carries the query (which
// is user input that could be a filename fragment) or an indexed node name. The
// query flows only as a bound LIKE parameter, which SQLite never echoes.
func TestSearchErrorPathFree(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := t.Context()
	const secretName = "SEARCH_secret_node_garply"
	const secretQuery = "SEARCH_secret_query_waldo"
	mustIndex(t, db, "repo", "snap", []model.BrowseNode{{Path: "/" + secretName, Name: secretName}})

	// Closing the pool forces QueryContext to fail; the wrapped error must stay
	// path-free.
	if err := db.pool.Close(); err != nil {
		t.Fatalf("pool close: %v", err)
	}
	_, err := db.Search(ctx, "repo", "snap", secretQuery, 0)
	assertErrPathFree(t, err, secretQuery, secretName)
}

// sameSet reports whether a and b contain the same strings regardless of order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
