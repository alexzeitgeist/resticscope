//go:build linux || darwin

package app

import "golang.org/x/sys/unix"

// canEnterDir reports real-UID search permission, matching a non-setuid child.
func canEnterDir(dir string) bool {
	return unix.Access(dir, unix.X_OK) == nil
}
