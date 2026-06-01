package resticx

import (
	"context"
	"encoding/json"
	"strings"

	"resticscope/internal/model"
)

// find.go runs `restic find --json --long [--host H] <pattern>` and parses the
// result. It is the second restic boundary the version view uses (alongside
// the cached snapshot list); the surrounding app/tui layers add no per-snapshot
// `ls` or `dump` call.

// resticFindLiteralPattern escapes the glob metacharacters Go's
// filepath.Match recognizes, so a literal path with any of [, ], *, ?, or \
// is not interpreted as a glob by `restic find`. Verified against restic
// 0.18.1: without escaping, `a[1].txt` matches `a1.txt` and `star*.txt`
// matches `starXYZ.txt`. The post-filter on Path == p in
// model.GroupFileVersions is the authoritative correctness gate; this escape
// is the cheap cost-saver that keeps restic from scanning matches that
// cannot possibly belong to this file.
func resticFindLiteralPattern(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		switch r {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FindMatches runs `restic find --json --long [--host H] <pattern>` and
// parses the result. host is empty to widen to all hosts. p is escaped so
// glob metacharacters in a literal filename do not over-match; the caller
// (model.GroupFileVersions) still post-filters Path == p as the
// authoritative gate. --no-lock is used for the same reason as Snapshots:
// read-only and we want to keep the repo usable when locks are write-held.
func (c *Client) FindMatches(ctx context.Context, t Target, creds Creds, host, p string) ([]model.FindSnapshotResult, error) {
	args := []string{"--no-lock", "find", "--json", "--long"}
	if host != "" {
		args = append(args, "--host", host)
	}
	args = append(args, resticFindLiteralPattern(p))
	out, err := c.runOp(ctx, t, creds, "find", args...)
	if err != nil {
		return nil, err
	}
	var rs []model.FindSnapshotResult
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, &Error{Kind: KindParse, Op: "find", wrapped: err}
	}
	return rs, nil
}
