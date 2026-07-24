package resticx

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// resticFindLiteralPattern escapes filepath.Match metacharacters for restic 0.18.1.
// This preserves literal matches and avoids overmatching; model.GroupFileVersions
// also enforces exact paths.
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

// FindMatches parses long JSON matches for p. An empty host searches all hosts;
// p is literal-escaped, and --no-lock permits reads while a write lock exists.
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
