//go:build unix

package browsedb

import (
	"errors"
	"os"
	"syscall"
)

// tryLock attempts a non-blocking exclusive advisory lock on f. ok reports
// whether the lock was acquired (false when another process holds it). supported
// is false when the host/filesystem reports flock as unavailable.
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

// unlock releases an advisory lock held on f. Closing f releases it too; this is
// used when cleanup briefly probes a lock it must not keep.
func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
