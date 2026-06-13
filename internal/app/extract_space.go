//go:build linux || darwin

package app

import (
	"math"

	"golang.org/x/sys/unix"
)

// freeBytesAt reports the bytes available to an unprivileged caller on the
// filesystem containing path (statfs Bavail, not Bfree — root-reserved blocks
// are excluded, matching what restic running as the user can actually write).
func freeBytesAt(path string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	// Clamp the product to avoid int64 overflow: Bavail is unsigned on
	// linux (uint64), and the multiplication would wrap when free space
	// exceeds ~8 EiB — advisory only; restic's own error is authoritative.
	bs := int64(st.Bsize)
	if bs > 0 && uint64(st.Bavail) > math.MaxInt64/uint64(bs) {
		return math.MaxInt64, true
	}
	return int64(st.Bavail) * bs, true
}
