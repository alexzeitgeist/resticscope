package browsedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionPrefix names the per-session browse directories under the cache dir.
// The lifecycle (lazy create on first browse, RemoveAll on clean exit) and the
// stale-cleanup convention both key off this single source of truth.
const SessionPrefix = "browse-session-"

// lockName is the marker/lock file created in every session directory. On
// supported platforms it carries a real advisory lock for the session's lifetime;
// elsewhere it is only a marker so the directory shape stays consistent.
const lockName = "active.lock"

// staleThreshold is how long a session directory must be untouched before
// cleanup will even consider removing it. It is deliberately generous to defend
// against surprising filesystem/lock semantics: a live session that is merely
// idle should never be reclaimed out from under a running TUI.
const staleThreshold = 3 * time.Hour

var errSessionLockUnavailable = errors.New("session lock unavailable")

// SessionLock holds the open lock file for a live session. On supported
// platforms it owns a non-blocking exclusive advisory lock; on unsupported
// platforms it is only a marker file. Close releases the lock and closes the file.
type SessionLock struct {
	f *os.File
}

// LockSession creates (or opens) the lock file in dir and, on supported
// platforms, takes a non-blocking exclusive lock held for the session lifetime.
// The file is created unconditionally for a consistent session-dir shape; on
// unsupported platforms no real lock is held, which only weakens stale-cleanup
// liveness detection (a documented disk leak, never a privacy bug since the key
// is gone). On platforms where locking is supported, acquisition failure is fatal
// because stale cleanup relies on that lock to identify live sessions.
func LockSession(dir string) (*SessionLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("browsedb lock: %w", pathFreeFSError(err))
	}
	ok, supported := tryLock(f)
	if supported && !ok {
		_ = f.Close()
		return nil, fmt.Errorf("browsedb lock: %w", errSessionLockUnavailable)
	}
	return &SessionLock{f: f}, nil
}

// Close releases the advisory lock (if held) and closes the lock file. It is
// safe to call on a nil lock or twice.
func (l *SessionLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlock(l.f)
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("browsedb unlock: %w", pathFreeFSError(err))
	}
	return nil
}

// CleanStaleSessions removes leftover browse-session-* directories under
// cacheDir, but only when they are demonstrably stale: untouched past
// staleThreshold AND not currently locked by a live session. If advisory locking
// is unsupported on this platform, every candidate is skipped rather than risk
// deleting a live sibling — so unsupported-platform builds accumulate stale
// (unreadable) session dirs, a documented disk leak. Best-effort: a removal error
// on one directory does not abort the sweep.
func CleanStaleSessions(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("browsedb clean: %w", pathFreeFSError(err))
	}
	now := time.Now()
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), SessionPrefix) {
			continue
		}
		dir := filepath.Join(cacheDir, e.Name())
		if !sessionIsStale(dir, now) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("browsedb clean: %w", pathFreeFSError(err))
		}
	}
	return firstErr
}

// sessionIsStale decides whether a session directory can be safely removed. It
// requires the directory to be untouched past staleThreshold and, on platforms
// that support locking, the session lock to be free. Any ambiguity (recent
// mtime, unsupported locking, lock held, lock file unreadable) is resolved as
// "not stale" so a live or uncertain session is never deleted.
func sessionIsStale(dir string, now time.Time) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	if now.Sub(info.ModTime()) < staleThreshold {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR, 0)
	if err != nil {
		// A sufficiently old session dir with no lock file at all is a crash
		// leftover from before the lock was created; anything else is ambiguous.
		return errors.Is(err, os.ErrNotExist)
	}
	defer f.Close()
	ok, supported := tryLock(f)
	if !supported || !ok {
		return false
	}
	unlock(f)
	return true
}
