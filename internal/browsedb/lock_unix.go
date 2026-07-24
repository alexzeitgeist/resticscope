//go:build unix

package browsedb

import (
	"errors"
	"os"
	"syscall"
)

// tryLock attempts a non-blocking exclusive advisory lock. supported is false
// when the host or filesystem reports flock as unavailable.
func tryLock(f *os.File) (ok, supported bool) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, true
	}
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTSUP) {
		return false, false
	}
	return false, true
}

// unlock releases a lock acquired only to probe whether a session is live.
func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
