package browsedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"resticscope/internal/model"
)

// listDirDIDQuery resolves a requested committed directory path to its did. A
// never-indexed snapshot or never-seen directory yields sql.ErrNoRows and an
// empty listing.
const listDirDIDQuery = `SELECT d.did FROM dirs d JOIN snapshots s ON s.sid=d.sid ` +
	`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND d.path=?`

// listDirQuery uses parent_did to fetch one directory's immediate children.
// Paths are reconstructed in Go from the requested parent path plus each row's
// name.
const listDirQuery = `SELECT name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target ` +
	`FROM nodes WHERE parent_did=? ` +
	`ORDER BY is_dir DESC, name_ci, name`

// searchQuery is the global filename search prefilter: a subsequence LIKE over
// the precomputed name_ci column across one committed snapshot, joined to dirs so
// each matched node carries its own parent path for full-path reconstruction. It
// deliberately has NO LIMIT and NO ORDER BY — the LIKE is only a coarse prefilter
// and the result cap is applied after fuzzy scoring (see Search), so the returned
// rows are the true top matches rather than an arbitrary prefix of the scan order.
// ESCAPE '\' makes the LIKE specials escaped by likeSubsequence match literally.
const searchQuery = `SELECT d.path,n.name,n.type,n.is_dir,n.size,n.mtime_unix,n.mtime_offset_sec,n.mtime_known,n.perms,n.uid,n.gid,n.owner_known,n.link_target ` +
	`FROM nodes n ` +
	`JOIN snapshots s ON s.sid=n.sid ` +
	`JOIN dirs d ON d.did=n.parent_did ` +
	`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND n.name_ci LIKE ? ESCAPE '\'`

// defaultSearchLimit caps how many ranked rows a search returns when the caller
// passes a non-positive limit. It bounds the result slice (and the UI state built
// from it) even when a broad query matches a large fraction of the snapshot; Total
// still reports every match so the UI can show "showing N of Total".
const defaultSearchLimit = 200

// IsIndexed reports whether (repo, snapshot) has a committed index marker.
func (db *DB) IsIndexed(ctx context.Context, repo, snapshot string) (bool, error) {
	var one int
	err := db.pool.QueryRowContext(ctx,
		`SELECT 1 FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`, repo, snapshot).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("browsedb is-indexed: %w", err)
	}
	return true, nil
}

// ListDir returns the immediate children of dir within (repo, snapshot),
// directories first then case-insensitively by name with deterministic
// tie-breakers. A never-indexed (repo, snapshot) yields an empty slice and nil
// error; callers that must distinguish "indexed but empty" use IsIndexed.
func (db *DB) ListDir(ctx context.Context, repo, snapshot, dir string) ([]model.BrowseEntry, error) {
	parent := model.CleanBrowsePath(dir)
	var did int64
	err := db.pool.QueryRowContext(ctx, listDirDIDQuery, repo, snapshot, parent).Scan(&did)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("browsedb list-dir: %w", err)
	}

	rows, err := db.pool.QueryContext(ctx, listDirQuery, did)
	if err != nil {
		return nil, fmt.Errorf("browsedb list-dir: %w", err)
	}
	defer rows.Close()

	var out []model.BrowseEntry
	var zones map[int]*time.Location
	for rows.Next() {
		var (
			e          model.BrowseEntry
			name       string
			isDir      int
			mtimeUnix  int64
			mtimeOff   int
			mtimeKnown int
			ownerKnown int
			uid, gid   int64
		)
		if err := rows.Scan(&name, &e.Type, &isDir, &e.Size,
			&mtimeUnix, &mtimeOff, &mtimeKnown, &e.Permissions, &uid, &gid, &ownerKnown, &e.LinkTarget); err != nil {
			return nil, fmt.Errorf("browsedb list-dir scan: %w", err)
		}
		e.Name = name
		// path is not stored; derive it from the requested parent and row name.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid)
		if mtimeKnown != 0 {
			// FixedZone preserves the wall-clock minute the old browse table
			// displayed; Location().String() is intentionally not meaningful.
			loc := zones[mtimeOff]
			if loc == nil {
				if zones == nil {
					zones = make(map[int]*time.Location, 2)
				}
				loc = time.FixedZone("", mtimeOff)
				zones[mtimeOff] = loc
			}
			e.ModTime = time.Unix(mtimeUnix, 0).In(loc)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("browsedb list-dir rows: %w", err)
	}
	if err := db.fillDirSizes(ctx, repo, snapshot, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Search finds nodes anywhere in (repo, snapshot) whose name is a case-folded
// fuzzy (subsequence) match for query, ranked best-first and capped to limit (the
// default cap when limit <= 0). It returns the ranked rows plus Total, the count
// of every match before the cap, so the caller can report a truncated result.
//
// A blank or whitespace-only query returns a zero result with no scan: there is
// nothing to rank. Otherwise the query is lowercased to build a subsequence LIKE
// pattern over name_ci (byte-identical to how name_ci was written, so the
// prefilter never rejects a true match), then every prefiltered row is re-scored
// with model.FuzzyScore on its original name and kept in a bounded top-N using the
// shared model.BetterFuzzy ordering — so DB search and the pure-model ranking
// cannot drift. The original query (not the lowercased pattern) is scored so the
// exact-case bonus still applies. Errors are path-free: they wrap the operation
// name and the driver/scan cause, never a node name or path (those flow only
// through bound parameters, which SQLite never echoes into error text).
func (db *DB) Search(ctx context.Context, repo, snapshot, query string, limit int) (model.BrowseSearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return model.BrowseSearchResult{}, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	pattern := likeSubsequence(strings.ToLower(query))

	rows, err := db.pool.QueryContext(ctx, searchQuery, repo, snapshot, pattern)
	if err != nil {
		return model.BrowseSearchResult{}, fmt.Errorf("browsedb search: %w", err)
	}
	defer rows.Close()

	var (
		top   []model.FuzzyRank // best-first, length bounded by limit
		total int
		zones map[int]*time.Location
	)
	for rows.Next() {
		var (
			e          model.BrowseEntry
			parent     string
			name       string
			isDir      int
			mtimeUnix  int64
			mtimeOff   int
			mtimeKnown int
			ownerKnown int
			uid, gid   int64
		)
		if err := rows.Scan(&parent, &name, &e.Type, &isDir, &e.Size,
			&mtimeUnix, &mtimeOff, &mtimeKnown, &e.Permissions, &uid, &gid, &ownerKnown, &e.LinkTarget); err != nil {
			return model.BrowseSearchResult{}, fmt.Errorf("browsedb search scan: %w", err)
		}
		match, ok := model.FuzzyScore(name, query)
		if !ok {
			// The LIKE prefilter and the subsequence test are equivalent conditions,
			// so this is defensive; skip anything the scorer rejects.
			continue
		}
		total++
		e.Name = name
		// path is not stored on the node; each row carries its own parent.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid)
		if mtimeKnown != 0 {
			loc := zones[mtimeOff]
			if loc == nil {
				if zones == nil {
					zones = make(map[int]*time.Location, 2)
				}
				loc = time.FixedZone("", mtimeOff)
				zones[mtimeOff] = loc
			}
			e.ModTime = time.Unix(mtimeUnix, 0).In(loc)
		}
		top = insertTopN(top, model.FuzzyRank{Entry: e, Match: match}, limit)
	}
	if err := rows.Err(); err != nil {
		return model.BrowseSearchResult{}, fmt.Errorf("browsedb search rows: %w", err)
	}

	out := make([]model.BrowseEntry, len(top))
	for i, r := range top {
		out[i] = r.Entry
	}
	if err := db.fillDirSizes(ctx, repo, snapshot, out); err != nil {
		return model.BrowseSearchResult{}, err
	}
	return model.BrowseSearchResult{Rows: out, Total: total}, nil
}

// dirSizePathChunk caps how many directory paths are bound into one dirSizes
// query. At one param per path plus the two fixed repo/snapshot binds, this stays
// far under the driver's LIMIT_VARIABLE_NUMBER even though callers (ListDir on a
// huge directory) may ask about thousands of children at once.
const dirSizePathChunk = 900

// fillDirSizes rewrites every directory entry's size in place to its recursive
// subtree total, leaving files untouched. A directory's raw nodes.size is its own
// (~0) inode size, so each dir is first zeroed and then filled from the dirs table;
// a directory absent from the lookup (empty subtree, or a never-indexed snapshot)
// keeps the explicit zero rather than leaking the inode value. ListDir and Search
// share this so the listing and search results stay consistent.
func (db *DB) fillDirSizes(ctx context.Context, repo, snapshot string, entries []model.BrowseEntry) error {
	dirPaths := make([]string, 0, len(entries))
	for i := range entries {
		if entries[i].IsDir {
			entries[i].Size = 0
			dirPaths = append(dirPaths, entries[i].Path)
		}
	}
	if len(dirPaths) == 0 {
		return nil
	}
	sizes, err := db.dirSizes(ctx, repo, snapshot, dirPaths)
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].IsDir {
			if sz, ok := sizes[entries[i].Path]; ok {
				entries[i].Size = sz
			}
		}
	}
	return nil
}

// dirSizes returns the recursive subtree size for each of paths within
// (repo, snapshot), keyed on the exact dirs.path the writer stored (the same
// model.JoinBrowsePath output ListDir/Search build), so no SQL path
// reconstruction is needed. Only directories with a positive subtree size appear
// in the map; an empty directory (or any path not present) is simply absent, and
// the caller leaves its size at the explicit zero it set. The path list is
// chunked to stay under the bind cap. A never-indexed snapshot yields an empty
// map.
func (db *DB) dirSizes(ctx context.Context, repo, snapshot string, paths []string) (map[string]int64, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make(map[string]int64, len(paths))
	for start := 0; start < len(paths); start += dirSizePathChunk {
		end := start + dirSizePathChunk
		if end > len(paths) {
			end = len(paths)
		}
		chunk := paths[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, repo, snapshot)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := db.pool.QueryContext(ctx, buildDirSizesSQL(len(chunk)), args...)
		if err != nil {
			return nil, fmt.Errorf("browsedb dir-sizes: %w", err)
		}
		for rows.Next() {
			var (
				p    string
				size int64
			)
			if err := rows.Scan(&p, &size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("browsedb dir-sizes scan: %w", err)
			}
			out[p] = size
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("browsedb dir-sizes rows: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// buildDirSizesSQL builds the dirSizes query for a chunk of n directory paths,
// with n bound placeholders in the IN clause after the fixed repo/snapshot binds.
func buildDirSizesSQL(n int) string {
	var b strings.Builder
	b.WriteString(`SELECT d.path,d.subtree_size FROM dirs d JOIN snapshots s ON s.sid=d.sid ` +
		`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND d.subtree_size>0 AND d.path IN (`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	b.WriteByte(')')
	return b.String()
}

// likeSubsequence builds a LIKE pattern matching q as a subsequence: each rune of
// q wrapped in '%' wildcards, so "abc" → "%a%b%c%". The LIKE specials (% _ \) are
// prefixed with a backslash so they match literally under ESCAPE '\'. q is the
// already-lowercased query, matching the BINARY-sorted name_ci column.
func likeSubsequence(q string) string {
	var b strings.Builder
	b.Grow(len(q)*2 + 1)
	b.WriteByte('%')
	for _, r := range q {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		b.WriteByte('%')
	}
	return b.String()
}

// insertTopN keeps top as a best-first slice of at most limit ranks, using the
// shared model.BetterFuzzy ordering. The slice stays sorted, so once it is full a
// candidate that cannot beat the current worst (the last element) is dropped in
// O(1); otherwise it is inserted at its ordered position and the worst is evicted.
// This bounds memory to limit even when a broad query matches a huge fraction of
// the snapshot, while still yielding the exact global top-N.
func insertTopN(top []model.FuzzyRank, r model.FuzzyRank, limit int) []model.FuzzyRank {
	if len(top) >= limit && !model.BetterFuzzy(r, top[len(top)-1]) {
		return top
	}
	// First position r outranks: the predicate is false…false,true…true because top
	// is sorted best-first, so sort.Search finds the correct insertion index.
	i := sort.Search(len(top), func(i int) bool { return model.BetterFuzzy(r, top[i]) })
	if len(top) < limit {
		top = append(top, model.FuzzyRank{})
		copy(top[i+1:], top[i:])
		top[i] = r
		return top
	}
	// Full: shift [i, len-1) down by one (dropping the old worst), then place r.
	copy(top[i+1:], top[i:len(top)-1])
	top[i] = r
	return top
}
