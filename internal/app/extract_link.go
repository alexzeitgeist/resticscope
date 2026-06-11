//go:build linux || darwin

package app

import "golang.org/x/sys/unix"

// linkNoFollow creates newname as a hard link to the node at oldname ITSELF —
// never to a symlink's target — failing with EEXIST instead of replacing.
// linux's link(2) already never follows, but darwin's does, so both platforms
// route through linkat(2) with AT_SYMLINK_FOLLOW clear. It is the one atomic
// no-replace publish primitive that covers every non-directory node type
// (regular file, symlink, fifo, device), and because it links the staged inode
// rather than re-creating the node, the snapshot metadata restic applied
// (mtime, ownership, mode) rides along untouched.
func linkNoFollow(oldname, newname string) error {
	return unix.Linkat(unix.AT_FDCWD, oldname, unix.AT_FDCWD, newname, 0)
}
