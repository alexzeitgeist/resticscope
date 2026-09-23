package resticx

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// ErrTreeNodeInvalidRequest rejects a lookup that restic could misread: an ID
// other than a full hex snapshot ID, an unclean directory, or a name that is
// not a single path element. It never echoes the rejected values.
var ErrTreeNodeInvalidRequest = errors.New("resticx: tree lookup needs a full snapshot ID, a clean directory, and a single name")

// TreeNode reads dir's tree object with `restic cat tree <snapshot>:<dir>` and
// returns its entry called name, the record restic compares when it diffs.
// found is false when dir has no such entry. A timeout of zero uses the
// client's default.
func (c *Client) TreeNode(ctx context.Context, t Target, creds Creds, snapshotID, dir, name string, timeout time.Duration) (node model.TreeNode, found bool, err error) {
	if assertCleanSnapshotID(snapshotID) != nil || !validTreeLookup(dir, name) {
		return model.TreeNode{}, false, ErrTreeNodeInvalidRequest
	}
	if timeout <= 0 {
		timeout = c.timeout()
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runner, err := c.streamRunner()
	if err != nil {
		return model.TreeNode{}, false, err
	}

	// A failing restic prints nothing, so the parse result is kept aside and the
	// process error, with its stderr, takes precedence.
	var parseErr error
	full := prependBackendOpts(t, "--no-lock", "cat", "tree", snapshotID+":"+dir)
	stderr, runErr := runner.RunStream(tctx, c.buildEnv(t, creds), creds.ResticPassword, func(r io.Reader) error {
		node, found, parseErr = model.FindTreeNode(r, name)
		return nil
	}, full...)

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return model.TreeNode{}, false, context.Canceled
	case runErr != nil:
		return model.TreeNode{}, false, c.classify(tctx, "cat", runErr, stderr)
	case parseErr != nil:
		return model.TreeNode{}, false, &Error{Kind: KindParse, Op: "cat", wrapped: parseErr}
	}
	return node, found, nil
}

// validTreeLookup accepts a cleaned absolute directory and one path element.
func validTreeLookup(dir, name string) bool {
	return dir == model.CleanBrowsePath(dir) && !strings.ContainsRune(dir, '\x00') &&
		name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}
