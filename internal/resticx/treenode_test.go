package resticx

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const treeNodeSnapID = "c7d8e9f09f4c2d1e8b7a6053f1e2d3c4b5a69788cc17d2e34a5b6c7d8e9f0a1b"

const treeNodeTree = `{"nodes":[` +
	`{"name":"group","type":"file","mode":420,"uid":0,"gid":0,"size":1034,"content":["aa11"]},` +
	`{"name":"passwd","type":"file","mode":384,"uid":0,"gid":0,"user":"root","size":2873,"content":["cc33"]}` +
	`]}`

func TestTreeNodeReadsParentTree(t *testing.T) {
	fs := &diffStreamFake{data: treeNodeTree}
	c := &Client{Stream: fs}
	n, found, err := c.TreeNode(t.Context(), testTarget, Creds{ResticPassword: "pw"}, treeNodeSnapID, "/etc", "passwd", time.Minute)
	if err != nil || !found {
		t.Fatalf("TreeNode = found %v, err %v", found, err)
	}
	if n.Name != "passwd" || n.Mode != 0o600 || n.User != "root" {
		t.Errorf("node = %+v", n)
	}
	want := []string{"--no-lock", "cat", "tree", treeNodeSnapID + ":/etc"}
	if len(fs.gotArgs) < len(want) || !slices.Equal(fs.gotArgs[len(fs.gotArgs)-len(want):], want) {
		t.Errorf("args %q, want suffix %q", fs.gotArgs, want)
	}
}

func TestTreeNodeRequiresStreamRunner(t *testing.T) {
	c := &Client{Runner: &fakeRunner{}}
	_, _, err := c.TreeNode(t.Context(), testTarget, Creds{}, treeNodeSnapID, "/etc", "passwd", time.Minute)
	if !errors.Is(err, ErrNoStreamRunner) {
		t.Errorf("err = %v, want ErrNoStreamRunner", err)
	}
}

func TestTreeNodeRootAndMissingEntry(t *testing.T) {
	fs := &diffStreamFake{data: treeNodeTree}
	c := &Client{Stream: fs}
	_, found, err := c.TreeNode(t.Context(), testTarget, Creds{}, treeNodeSnapID, "/", "shadow", 0)
	if err != nil || found {
		t.Errorf("TreeNode = found %v, err %v, want not found", found, err)
	}
	if got := fs.gotArgs[len(fs.gotArgs)-1]; got != treeNodeSnapID+":/" {
		t.Errorf("root lookup arg = %q", got)
	}
}

// Anything restic could resolve or parse differently is refused before restic
// runs, and the refusal names none of the inputs.
func TestTreeNodeRejectsUnsafeRequests(t *testing.T) {
	for _, tc := range []struct{ id, dir, name string }{
		{"latest", "/etc", "passwd"},
		{"c7d8e9f0", "/etc", "passwd"},
		{"-" + treeNodeSnapID[1:], "/etc", "passwd"},
		{treeNodeSnapID, "etc", "passwd"},
		{treeNodeSnapID, "/etc/../root", "passwd"},
		{treeNodeSnapID, "/etc/", "passwd"},
		{treeNodeSnapID, "/etc", ""},
		{treeNodeSnapID, "/etc", ".."},
		{treeNodeSnapID, "/etc", "a/b"},
		{treeNodeSnapID, "/etc", "a\x00b"},
	} {
		fs := &diffStreamFake{data: treeNodeTree}
		c := &Client{Stream: fs}
		_, _, err := c.TreeNode(t.Context(), testTarget, Creds{}, tc.id, tc.dir, tc.name, time.Minute)
		if !errors.Is(err, ErrTreeNodeInvalidRequest) {
			t.Errorf("TreeNode(%q, %q, %q) err = %v, want ErrTreeNodeInvalidRequest", tc.id, tc.dir, tc.name, err)
		}
		if fs.gotArgs != nil {
			t.Errorf("TreeNode(%q, %q, %q) ran restic", tc.id, tc.dir, tc.name)
		}
	}
}

// restic prints nothing on stdout when it fails, so its exit status and stderr
// must win over the empty-output parse failure.
func TestTreeNodeResticFailureWinsOverParse(t *testing.T) {
	fs := &diffStreamFake{err: fakeExitError(1), stderr: []byte("Fatal: path etc: not found")}
	c := &Client{Stream: fs}
	_, _, err := c.TreeNode(t.Context(), testTarget, Creds{}, treeNodeSnapID, "/etc", "passwd", time.Minute)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindUnknown || re.Op != "cat" || !strings.Contains(re.Stderr, "not found") {
		t.Fatalf("err = %#v, want an unknown cat failure carrying stderr", err)
	}
}

func TestTreeNodeMalformedOutputIsParseError(t *testing.T) {
	fs := &diffStreamFake{data: "repository opened\n"}
	c := &Client{Stream: fs}
	_, _, err := c.TreeNode(t.Context(), testTarget, Creds{}, treeNodeSnapID, "/etc", "passwd", time.Minute)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindParse {
		t.Fatalf("err = %v, want KindParse", err)
	}
}

func TestTreeNodeCancellation(t *testing.T) {
	fs := &diffStreamFake{block: true}
	c := &Client{Stream: fs}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := c.TreeNode(ctx, testTarget, Creds{}, treeNodeSnapID, "/etc", "passwd", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestTreeNodeTimeoutClassifies(t *testing.T) {
	fs := &diffStreamFake{block: true}
	c := &Client{Stream: fs}
	_, _, err := c.TreeNode(t.Context(), testTarget, Creds{}, treeNodeSnapID, "/etc", "passwd", time.Millisecond)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Errorf("err = %v, want KindTimeout", err)
	}
}
