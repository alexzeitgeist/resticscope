//go:build !unix

package browsedb

import "os"

// tryLock reports that advisory locking is unsupported on this platform.
// Stale-session cleanup then skips every candidate rather than guess at lock
// semantics, so old (unreadable) session dirs may accumulate — a documented
// disk leak that is harmless to privacy because the encryption key is gone.
func tryLock(*os.File) (ok, supported bool) {
	return false, false
}

// unlock is a no-op where advisory locking is unsupported.
func unlock(*os.File) {}
