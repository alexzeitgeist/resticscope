package browsedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
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

// searchQuery prefilters one committed snapshot by a subsequence LIKE pattern.
// It has no ordering or limit because Search ranks the complete match set.
// Joining dirs supplies parent paths, and ESCAPE makes LIKE metacharacters
// produced by likeSubsequence literal.
const searchQuery = `SELECT d.path,n.name,n.type,n.is_dir,n.size,n.mtime_unix,n.mtime_offset_sec,n.mtime_known,n.perms,n.uid,n.gid,n.owner_known,n.link_target ` +
	`FROM nodes n ` +
	`JOIN snapshots s ON s.sid=n.sid ` +
	`JOIN dirs d ON d.did=n.parent_did ` +
	`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND n.name_ci LIKE ? ESCAPE '\'`

// defaultSearchLimit bounds returned rows for non-positive limits. Total remains
// uncapped so callers can report truncated results.
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

// subtreeSIDQuery resolves (repo, snapshot) to its committed sid; sql.ErrNoRows
// means the snapshot has no committed index.
const subtreeSIDQuery = `SELECT sid FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`

// subtreeCountsQuery uses a half-open BINARY path range to count descendants.
// Unlike LIKE, the range is case-sensitive and treats wildcard bytes literally.
const subtreeCountsQuery = `SELECT ` +
	`COALESCE(SUM(CASE WHEN n.type='file' THEN 1 ELSE 0 END),0),` +
	`COALESCE(SUM(CASE WHEN n.is_dir THEN 1 ELSE 0 END),0) ` +
	`FROM nodes n JOIN dirs d ON d.did=n.parent_did ` +
	`WHERE n.sid=? AND d.sid=? AND (d.path=? OR (d.path>=? AND d.path<?))`

// SubtreeCounts reports recursive file and directory counts below dir, excluding
// dir itself, symlinks, and special nodes. known is false when the snapshot has
// no committed index.
func (db *DB) SubtreeCounts(ctx context.Context, repo, snapshot, dir string) (files, dirs int, known bool, err error) {
	root := model.CleanBrowsePath(dir)
	var sid int64
	err = db.pool.QueryRowContext(ctx, subtreeSIDQuery, repo, snapshot).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("browsedb subtree-counts: %w", err)
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	// "starts with prefix" as a half-open byte range; '0' is the byte after '/'.
	upper := prefix[:len(prefix)-1] + "0"
	if err := db.pool.QueryRowContext(ctx, subtreeCountsQuery,
		sid, sid, root, prefix, upper).Scan(&files, &dirs); err != nil {
		return 0, 0, false, fmt.Errorf("browsedb subtree-counts: %w", err)
	}
	return files, dirs, true, nil
}

// ListDir returns the immediate children of dir, with directories first and
// names ordered case-insensitively with deterministic tie-breakers. A snapshot
// without a committed index returns an empty slice and nil error.
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
		// Nodes omit paths, so reconstruct one from the requested parent.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid) //nolint:gosec // values originate from uint32, no truncation
		if mtimeKnown != 0 {
			// Preserve the source wall-clock time; the location name is irrelevant.
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

// Search returns the best case-folded subsequence name matches, capped at limit
// or the default for non-positive limits. Total counts all matches before the cap.
// A blank query performs no scan. Ranking uses the original query to preserve
// exact-case bonuses and follows model.BetterFuzzy. Errors omit node names and paths.
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
			// Defensively reject any row admitted by LIKE but rejected by the scorer.
			continue
		}
		total++
		e.Name = name
		// Reconstruct the omitted path from the joined parent.
		e.Path = model.JoinBrowsePath(parent, name)
		e.IsDir = isDir != 0
		e.OwnerKnown = ownerKnown != 0
		e.UID, e.GID = uint32(uid), uint32(gid) //nolint:gosec // values originate from uint32, no truncation
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

// dirSizePathChunk keeps directory-size queries below the driver's bind limit.
const dirSizePathChunk = 900

// fillDirSizes replaces directory sizes with recursive totals and leaves files
// unchanged. Missing or empty directories remain zero rather than exposing raw
// inode sizes.
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

// dirSizes returns positive recursive sizes keyed by exact stored paths.
// Absent paths remain zero in the caller; queries are chunked below the bind cap.
func (db *DB) dirSizes(ctx context.Context, repo, snapshot string, paths []string) (map[string]int64, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make(map[string]int64, len(paths))
	for start := 0; start < len(paths); start += dirSizePathChunk {
		end := min(start+dirSizePathChunk, len(paths))
		if err := db.dirSizesChunk(ctx, repo, snapshot, paths[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// dirSizesChunk writes one query chunk into out. Its local defer closes the rows
// before dirSizes opens the next chunk.
func (db *DB) dirSizesChunk(ctx context.Context, repo, snapshot string, chunk []string, out map[string]int64) error {
	args := make([]any, 0, len(chunk)+2)
	args = append(args, repo, snapshot)
	for _, p := range chunk {
		args = append(args, p)
	}
	rows, err := db.pool.QueryContext(ctx, buildDirSizesSQL(len(chunk)), args...)
	if err != nil {
		return fmt.Errorf("browsedb dir-sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p    string
			size int64
		)
		if err := rows.Scan(&p, &size); err != nil {
			return fmt.Errorf("browsedb dir-sizes scan: %w", err)
		}
		out[p] = size
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("browsedb dir-sizes rows: %w", err)
	}
	return nil
}

// buildDirSizesSQL builds a directory-size query with n path binds.
func buildDirSizesSQL(n int) string {
	var b strings.Builder
	b.WriteString(`SELECT d.path,d.subtree_size FROM dirs d JOIN snapshots s ON s.sid=d.sid ` +
		`WHERE s.repo=? AND s.snapshot=? AND s.indexed_at_unix IS NOT NULL AND d.subtree_size>0 AND d.path IN (`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	b.WriteByte(')')
	return b.String()
}

// likeSubsequence builds a LIKE subsequence pattern from lowercase q. It escapes
// LIKE metacharacters so they match literally under the query's ESCAPE clause.
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

// insertTopN maintains at most limit ranks in model.BetterFuzzy order. Candidates
// that cannot beat a full slice's worst entry are discarded immediately.
func insertTopN(top []model.FuzzyRank, r model.FuzzyRank, limit int) []model.FuzzyRank {
	if len(top) >= limit && !model.BetterFuzzy(r, top[len(top)-1]) {
		return top
	}
	// Search for the first entry that r outranks in the best-first slice.
	i := sort.Search(len(top), func(i int) bool { return model.BetterFuzzy(r, top[i]) })
	if len(top) < limit {
		top = append(top, model.FuzzyRank{})
		copy(top[i+1:], top[i:])
		top[i] = r
		return top
	}
	copy(top[i+1:], top[i:len(top)-1])
	top[i] = r
	return top
}
