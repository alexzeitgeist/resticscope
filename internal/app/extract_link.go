//go:build linux || darwin

package app

import "golang.org/x/sys/unix"

// linkNoFollow atomically hard-links a non-directory node without following
// symlinks or replacing the target. Darwin's link follows a final symlink, so
// linkat with AT_SYMLINK_FOLLOW clear provides that behavior on Linux and Darwin
// while preserving the staged inode's metadata.
func linkNoFollow(oldname, newname string) error {
	return unix.Linkat(unix.AT_FDCWD, oldname, unix.AT_FDCWD, newname, 0)
}
