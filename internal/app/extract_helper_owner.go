//go:build linux || darwin

package app

import (
	"io/fs"
	"syscall"
)

// ownedByUID reports whether the Lstat'd node belongs to uid. It backs the
// helper's shared-cache authorization (prepareHelperCache), so it must answer
// from the real inode owner, never from permission bits.
func ownedByUID(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
