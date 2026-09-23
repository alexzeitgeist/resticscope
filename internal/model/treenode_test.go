package model

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// treeFixture follows restic's tree encoding: names escaped with strconv.Quote,
// modes as os.FileMode numbers, and a link target restic could not store as
// UTF-8 moved to base64 linktarget_raw.
const treeFixture = `{"nodes":[
{"name":"group","type":"file","mode":420,"mtime":"2026-04-02T09:14:07.5+02:00","atime":"2026-04-02T09:14:07.5+02:00","ctime":"2026-04-02T09:14:07.5+02:00","uid":0,"gid":0,"user":"root","group":"root","inode":1310,"size":1034,"links":1,"content":["aa11"]},
{"name":"nginx","type":"dir","mode":2147484141,"mtime":"2026-05-01T10:00:00Z","atime":"2026-05-01T10:00:00Z","ctime":"2026-05-01T10:00:00Z","uid":0,"gid":0,"user":"root","group":"root","inode":1400,"content":null,"subtree":"bb22"},
{"name":"odd\\\"name","type":"symlink","mode":134218239,"mtime":"2026-05-01T10:00:00Z","atime":"2026-05-01T10:00:00Z","ctime":"2026-05-01T10:00:00Z","uid":0,"gid":0,"inode":1500,"linktarget_raw":"/3RhcmdldA==","content":null},
{"name":"passwd","type":"file","mode":384,"mtime":"2026-04-02T09:14:07Z","atime":"2026-04-02T09:14:07Z","ctime":"2026-05-22T18:40:51Z","uid":0,"gid":0,"user":"root","group":"root","inode":1311,"size":2873,"links":1,"content":["cc33","dd44"],"extended_attributes":[{"name":"security.selinux","value":"c3lzdGVt"}]}
]}`

func TestFindTreeNodeMatchesEntry(t *testing.T) {
	n, found, err := FindTreeNode(strings.NewReader(treeFixture), "passwd")
	if err != nil || !found {
		t.Fatalf("FindTreeNode = found %v, err %v", found, err)
	}
	if n.Name != "passwd" || n.Type != NodeTypeFile || n.Mode != 0o600 || n.Size != 2873 || n.Inode != 1311 {
		t.Errorf("node = %+v", n)
	}
	if n.User != "root" || n.UID != 0 || n.Links != 1 {
		t.Errorf("ownership = %q/%d links %d", n.User, n.UID, n.Links)
	}
	if want := time.Date(2026, 5, 22, 18, 40, 51, 0, time.UTC); !n.ChangeTime.Equal(want) {
		t.Errorf("ctime = %v, want %v", n.ChangeTime, want)
	}
	if !slices.Equal(n.Content, []string{"cc33", "dd44"}) {
		t.Errorf("content = %q", n.Content)
	}
	if len(n.Xattrs) != 1 || n.Xattrs[0].Name != "security.selinux" || string(n.Xattrs[0].Value) != "system" {
		t.Errorf("xattrs = %+v", n.Xattrs)
	}
}

func TestFindTreeNodeDirectoryAndSymlink(t *testing.T) {
	dir, found, err := FindTreeNode(strings.NewReader(treeFixture), "nginx")
	if err != nil || !found {
		t.Fatalf("nginx: found %v, err %v", found, err)
	}
	if dir.Mode != os.ModeDir|0o755 || dir.Subtree != "bb22" || dir.Content != nil {
		t.Errorf("dir = %+v", dir)
	}

	// The stored name is escaped; callers match the real name.
	link, found, err := FindTreeNode(strings.NewReader(treeFixture), `odd"name`)
	if err != nil || !found {
		t.Fatalf(`odd"name: found %v, err %v`, found, err)
	}
	if link.Name != `odd"name` || link.LinkTarget != "\xfftarget" {
		t.Errorf("symlink name %q target %q", link.Name, link.LinkTarget)
	}
}

func TestFindTreeNodeMissingEntry(t *testing.T) {
	_, found, err := FindTreeNode(strings.NewReader(treeFixture), "shadow")
	if err != nil || found {
		t.Errorf("FindTreeNode = found %v, err %v, want not found", found, err)
	}
}

// restic accepts unknown keys around "nodes" for forward compatibility.
func TestFindTreeNodeSkipsUnknownKeys(t *testing.T) {
	in := `{"version":2,"meta":{"x":[1,2]},"nodes":[{"name":"a","type":"file"}],"after":true}`
	n, found, err := FindTreeNode(strings.NewReader(in), "a")
	if err != nil || !found || n.Type != NodeTypeFile {
		t.Errorf("FindTreeNode = %+v found %v err %v", n, found, err)
	}
}

func TestFindTreeNodeRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		`[]`,
		`{"other":1}`,
		`{"nodes":{}}`,
		`{"nodes":[{"name":"a","mode":"rw"}]}`,
		`{"nodes":[{"name":`,
	} {
		if _, _, err := FindTreeNode(strings.NewReader(in), "a"); err == nil {
			t.Errorf("FindTreeNode(%q) succeeded, want an error", in)
		}
	}
}

func TestCompareTreeNodes(t *testing.T) {
	base := TreeNode{
		Name: "f", Type: NodeTypeFile, Mode: 0o644, UID: 0, User: "root", GID: 0, Group: "root",
		ModTime: time.Unix(100, 0), AccessTime: time.Unix(100, 0), ChangeTime: time.Unix(100, 0),
		Inode: 7, Size: 3, Links: 1, Content: []string{"a"},
		Xattrs: []ExtendedAttribute{{Name: "user.a", Value: []byte("1")}, {Name: "user.b", Value: []byte("2")}},
	}
	if got := CompareTreeNodes(base, base); got.Any() {
		t.Errorf("identical nodes differ: %+v", got)
	}

	other := base
	other.Mode = 0o600
	other.User = "alex"
	other.ChangeTime = time.Unix(200, 0)
	// Same instant in another zone is not a change.
	other.ModTime = time.Unix(100, 0).In(time.FixedZone("x", 3600))
	other.Xattrs = []ExtendedAttribute{{Name: "user.b", Value: []byte("2")}, {Name: "user.a", Value: []byte("1")}}
	want := NodeChanges{Mode: true, Owner: true, ChangeTime: true}
	if got := CompareTreeNodes(base, other); got != want {
		t.Errorf("CompareTreeNodes = %+v, want %+v", got, want)
	}

	empty := base
	empty.Content = []string{}
	nilContent := base
	nilContent.Content = nil
	if got := CompareTreeNodes(empty, nilContent); !got.Content {
		t.Error("an empty content list must differ from a missing one, as in restic")
	}

	dirA := TreeNode{Type: NodeTypeDir, Subtree: "aa"}
	dirB := TreeNode{Type: NodeTypeDir, Subtree: "bb"}
	got := CompareTreeNodes(dirA, dirB)
	if !got.Tree || got.OwnFieldsOnly().Any() {
		t.Errorf("subtree-only change = %+v, want Tree alone", got)
	}
}

func TestCompareTreeNodesGenericAttrs(t *testing.T) {
	a := TreeNode{GenericAttrs: map[string]json.RawMessage{"windows.file_attributes": json.RawMessage(`32`)}}
	b := TreeNode{GenericAttrs: map[string]json.RawMessage{"windows.file_attributes": json.RawMessage(`33`)}}
	if !CompareTreeNodes(a, b).GenericAttrs {
		t.Error("changed generic attribute value not detected")
	}
	if CompareTreeNodes(a, a).GenericAttrs {
		t.Error("identical generic attributes reported as changed")
	}
}

func TestXattrValuesDiffer(t *testing.T) {
	a := []ExtendedAttribute{{Name: "user.z", Value: []byte("1")}, {Name: "user.b", Value: []byte("2")}, {Name: "user.a"}, {Name: "user.gone"}}
	b := []ExtendedAttribute{{Name: "user.b", Value: []byte("3")}, {Name: "user.a", Value: []byte("x")}, {Name: "user.z", Value: []byte("9")}, {Name: "user.new"}}
	// Sorted rather than in a's stored order; added and removed names are not listed.
	if got, want := XattrValuesDiffer(a, b), []string{"user.a", "user.b", "user.z"}; !slices.Equal(got, want) {
		t.Errorf("XattrValuesDiffer = %q, want %q", got, want)
	}
}

func TestGenericAttrValuesDiffer(t *testing.T) {
	a := map[string]json.RawMessage{
		"windows.file_attributes": json.RawMessage(`32`),
		"windows.creation_time":   json.RawMessage(`"AA"`),
		"windows.same":            json.RawMessage(`1`),
		"windows.gone":            json.RawMessage(`1`),
	}
	b := map[string]json.RawMessage{
		"windows.file_attributes": json.RawMessage(`33`),
		"windows.creation_time":   json.RawMessage(`"AB"`),
		"windows.same":            json.RawMessage(`1`),
		"windows.new":             json.RawMessage(`1`),
	}
	if got, want := GenericAttrValuesDiffer(a, b), []string{"windows.creation_time", "windows.file_attributes"}; !slices.Equal(got, want) {
		t.Errorf("GenericAttrValuesDiffer = %q, want %q", got, want)
	}
	if got := GenericAttrValuesDiffer(a, nil); got != nil {
		t.Errorf("GenericAttrValuesDiffer with nothing in common = %q, want nil", got)
	}
}
