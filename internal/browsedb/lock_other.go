//go:build !unix && !windows

package browsedb

import "os"

// tryLock reports that advisory locking is unsupported on this platform.
// Cleanup cannot classify sessions with lock markers as stale, so abandoned
// encrypted directories may accumulate after their in-memory keys are lost.
func tryLock(*os.File) (ok, supported bool) {
	return false, false
}

// unlock is a no-op where advisory locking is unsupported.
func unlock(*os.File) {}
