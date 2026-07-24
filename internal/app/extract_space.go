//go:build linux || darwin

package app

import (
	"math"

	"golang.org/x/sys/unix"
)

// freeBytesAt reports the bytes statfs marks available to non-superusers on the
// filesystem containing path.
func freeBytesAt(path string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	// Clamp overflow above roughly 8 EiB; the result is advisory to restic.
	bs := int64(st.Bsize)                                       //nolint:unconvert // Bsize is uint32 on darwin; the cast is needed cross-platform
	if bs > 0 && uint64(st.Bavail) > math.MaxInt64/uint64(bs) { //nolint:unconvert // normalizes Bavail across platforms where its type varies
		return math.MaxInt64, true
	}
	return int64(st.Bavail) * bs, true //nolint:gosec // the clamp above guarantees the product fits in int64
}
