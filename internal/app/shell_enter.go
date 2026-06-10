//go:build linux || darwin

package app

import "golang.org/x/sys/unix"

// canEnterDir reports whether the user has search permission on dir — the
// same check the launched shell's chdir will face. access(2) checks the real
// uid, which matches the child exactly: resticscope never runs setuid.
func canEnterDir(dir string) bool {
	return unix.Access(dir, unix.X_OK) == nil
}
