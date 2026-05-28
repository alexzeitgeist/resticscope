//go:build windows

package browsedb

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const (
	lockBytesLow  = 1
	lockBytesHigh = 0
)

// tryLock attempts a non-blocking exclusive byte-range lock on f. ok reports
// whether the lock was acquired (false when another process holds it). supported
// is false when the host/filesystem reports byte-range locking as unavailable.
func tryLock(f *os.File) (ok, supported bool) {
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockBytesLow,
		lockBytesHigh,
		&windows.Overlapped{},
	)
	if err == nil {
		return true, true
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_FAILED) {
		return false, true
	}
	if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return false, false
	}
	return false, true
}

// unlock releases the byte-range lock held on f. Closing f releases it too; this
// is used when cleanup briefly probes a lock it must not keep.
func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockBytesLow, lockBytesHigh, &windows.Overlapped{})
}
