package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"
)

// Tree nodes are restic's own records for one path, read with `restic cat
// tree`. They carry every field restic compares when it diffs, so the diff info
// view can name what changed. Like diff paths, they stay in the TUI session.

// TreeNode is one entry of a restic tree object. Name is unescaped the way
// restic stores it, and LinkTarget already includes restic's raw-byte fallback.
type TreeNode struct {
	Name         string
	Type         string
	Mode         os.FileMode
	ModTime      time.Time
	AccessTime   time.Time
	ChangeTime   time.Time
	UID, GID     uint32
	User, Group  string
	Inode        uint64
	DeviceID     uint64
	Size         uint64
	Links        uint64
	LinkTarget   string
	Device       uint64
	Xattrs       []ExtendedAttribute
	GenericAttrs map[string]json.RawMessage
	Content      []string
	Subtree      string
	Error        string
}

// ExtendedAttribute is one extended attribute. Values can hold anything the
// filesystem stored, so renderers show names only.
type ExtendedAttribute struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

// treeNodeJSON mirrors restic's node encoding in internal/data/node.go.
type treeNodeJSON struct {
	Name          string                     `json:"name"`
	Type          string                     `json:"type"`
	Mode          os.FileMode                `json:"mode"`
	ModTime       time.Time                  `json:"mtime"`
	AccessTime    time.Time                  `json:"atime"`
	ChangeTime    time.Time                  `json:"ctime"`
	UID           uint32                     `json:"uid"`
	GID           uint32                     `json:"gid"`
	User          string                     `json:"user"`
	Group         string                     `json:"group"`
	Inode         uint64                     `json:"inode"`
	DeviceID      uint64                     `json:"device_id"`
	Size          uint64                     `json:"size"`
	Links         uint64                     `json:"links"`
	LinkTarget    string                     `json:"linktarget"`
	LinkTargetRaw []byte                     `json:"linktarget_raw"`
	Xattrs        []ExtendedAttribute        `json:"extended_attributes"`
	GenericAttrs  map[string]json.RawMessage `json:"generic_attributes"`
	Device        uint64                     `json:"device"`
	Content       []string                   `json:"content"`
	Subtree       string                     `json:"subtree"`
	Error         string                     `json:"error"`
}

// errTreeFormat reports a tree object that is not restic's `{"nodes":[...]}`.
var errTreeFormat = errors.New("tree object is not a JSON object with a nodes array")

// FindTreeNode streams a tree object from r and returns the entry called name,
// stopping at the match. found is false when the tree has no such entry. Like
// restic, it skips unknown keys around "nodes".
func FindTreeNode(r io.Reader, name string) (node TreeNode, found bool, err error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	if err := expectDelim(dec, '{'); err != nil {
		return TreeNode{}, false, err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return TreeNode{}, false, err
		}
		if key, _ := tok.(string); key != "nodes" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return TreeNode{}, false, err
			}
			continue
		}
		return findNodeInArray(dec, name)
	}
	return TreeNode{}, false, errTreeFormat
}

// findNodeInArray decodes only the name of each entry until one matches.
func findNodeInArray(dec *json.Decoder, name string) (TreeNode, bool, error) {
	if err := expectDelim(dec, '['); err != nil {
		return TreeNode{}, false, err
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return TreeNode{}, false, err
		}
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return TreeNode{}, false, err
		}
		if unquoteNodeName(head.Name) != name {
			continue
		}
		var n treeNodeJSON
		if err := json.Unmarshal(raw, &n); err != nil {
			return TreeNode{}, false, err
		}
		return n.node(), true, nil
	}
	return TreeNode{}, false, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: got %v, want %v", errTreeFormat, tok, want)
	}
	return nil
}

// unquoteNodeName reverses restic's strconv.Quote escaping of stored names. A
// name that does not unquote is compared as stored.
func unquoteNodeName(s string) string {
	if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return u
	}
	return s
}

func (n treeNodeJSON) node() TreeNode {
	target := n.LinkTarget
	if n.LinkTargetRaw != nil {
		target = string(n.LinkTargetRaw)
	}
	return TreeNode{
		Name:         unquoteNodeName(n.Name),
		Type:         n.Type,
		Mode:         n.Mode,
		ModTime:      n.ModTime,
		AccessTime:   n.AccessTime,
		ChangeTime:   n.ChangeTime,
		UID:          n.UID,
		GID:          n.GID,
		User:         n.User,
		Group:        n.Group,
		Inode:        n.Inode,
		DeviceID:     n.DeviceID,
		Size:         n.Size,
		Links:        n.Links,
		LinkTarget:   target,
		Device:       n.Device,
		Xattrs:       n.Xattrs,
		GenericAttrs: n.GenericAttrs,
		Content:      n.Content,
		Subtree:      n.Subtree,
		Error:        n.Error,
	}
}

// NodeChanges flags the fields that differ between two records of one path.
// Together they cover restic's Node.Equals apart from the name, which the
// lookup already matched.
type NodeChanges struct {
	Type, Mode, Owner, Group            bool
	ModTime, AccessTime, ChangeTime     bool
	Inode, DeviceID, Size, Links        bool
	LinkTarget, Device                  bool
	Xattrs, GenericAttrs, Content, Tree bool
	Error                               bool
}

// CompareTreeNodes reports which fields of a and b differ. Owner and Group
// cover both the numeric ID and the name; Tree compares directory subtrees.
func CompareTreeNodes(a, b TreeNode) NodeChanges {
	return NodeChanges{
		Type:         a.Type != b.Type,
		Mode:         a.Mode != b.Mode,
		Owner:        a.UID != b.UID || a.User != b.User,
		Group:        a.GID != b.GID || a.Group != b.Group,
		ModTime:      !a.ModTime.Equal(b.ModTime),
		AccessTime:   !a.AccessTime.Equal(b.AccessTime),
		ChangeTime:   !a.ChangeTime.Equal(b.ChangeTime),
		Inode:        a.Inode != b.Inode,
		DeviceID:     a.DeviceID != b.DeviceID,
		Size:         a.Size != b.Size,
		Links:        a.Links != b.Links,
		LinkTarget:   a.LinkTarget != b.LinkTarget,
		Device:       a.Device != b.Device,
		Xattrs:       !sameXattrs(a.Xattrs, b.Xattrs),
		GenericAttrs: !sameGenericAttrs(a.GenericAttrs, b.GenericAttrs),
		// restic treats a missing content list as different from an empty one.
		Content: (a.Content == nil) != (b.Content == nil) || !slices.Equal(a.Content, b.Content),
		Tree:    a.Subtree != b.Subtree,
		Error:   a.Error != b.Error,
	}
}

// OwnFieldsOnly returns c without the content and subtree comparisons, leaving
// the node's own metadata.
func (c NodeChanges) OwnFieldsOnly() NodeChanges {
	c.Content, c.Tree = false, false
	return c
}

// Any reports whether any field differs.
func (c NodeChanges) Any() bool { return c != NodeChanges{} }

// sameXattrs compares attributes by name, ignoring order.
func sameXattrs(a, b []ExtendedAttribute) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string][]byte, len(a))
	for _, x := range a {
		byName[x.Name] = x.Value
	}
	for _, x := range b {
		v, ok := byName[x.Name]
		if !ok || !bytes.Equal(v, x.Value) {
			return false
		}
	}
	return true
}

func sameGenericAttrs(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

// XattrValuesDiffer lists, sorted, the extended attributes present in both a
// and b whose values differ.
func XattrValuesDiffer(a, b []ExtendedAttribute) []string {
	byName := make(map[string][]byte, len(b))
	for _, x := range b {
		byName[x.Name] = x.Value
	}
	var out []string
	for _, x := range a {
		if v, ok := byName[x.Name]; ok && !bytes.Equal(v, x.Value) {
			out = append(out, x.Name)
		}
	}
	slices.Sort(out)
	return out
}

// GenericAttrValuesDiffer lists, sorted, the generic attributes present in both
// a and b whose raw values differ, the byte comparison restic makes.
func GenericAttrValuesDiffer(a, b map[string]json.RawMessage) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; ok && !bytes.Equal(v, w) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
