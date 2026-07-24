//go:build linux || darwin

package app

import (
	"io/fs"
	"syscall"
)

// ownedByUID reports whether the Lstat inode owner matches uid for cache
// authorization.
func ownedByUID(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
