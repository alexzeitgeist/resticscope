//go:build linux || darwin

package app

import "golang.org/x/sys/unix"

// freeBytesAt reports the bytes available to an unprivileged caller on the
// filesystem containing path (statfs Bavail, not Bfree — root-reserved blocks
// are excluded, matching what restic running as the user can actually write).
func freeBytesAt(path string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
