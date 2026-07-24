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

// tryLock attempts a non-blocking exclusive byte-range lock. supported is false
// when the host or filesystem reports locking as unavailable.
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

// unlock releases a lock acquired only to probe whether a session is live.
func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockBytesLow, lockBytesHigh, &windows.Overlapped{})
}
