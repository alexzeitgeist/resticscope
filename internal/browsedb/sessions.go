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
// They are created lazily, removed on clean shutdown, and scavenged by
// CleanStaleSessions.
const SessionPrefix = "browse-session-"

// lockName is every session's advisory lock file or unsupported-platform marker.
const lockName = "active.lock"

// staleThreshold is the minimum inactivity before cleanup considers a session.
const staleThreshold = 3 * time.Hour

var errSessionLockUnavailable = errors.New("session lock unavailable")

// SessionLock owns a live session's advisory lock or unsupported-platform marker.
// Close releases the lock and closes the file.
type SessionLock struct {
	f *os.File
}

// LockSession opens the session marker and, where supported, holds a non-blocking
// exclusive lock until Close. Unsupported locking may leak unreadable stale
// directories. A supported platform must acquire the lock because cleanup uses it
// to distinguish live sessions.
func LockSession(dir string) (*SessionLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is an internal session dir joined with a constant lock filename
	if err != nil {
		return nil, fmt.Errorf("browsedb lock: %w", PathFreeFSError(err))
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
		return fmt.Errorf("browsedb unlock: %w", PathFreeFSError(err))
	}
	return nil
}

// CleanStaleSessions removes old, unlocked session directories under cacheDir.
// Unsupported locking retains candidates with a lock marker. Removal is
// best-effort and returns only the first error.
func CleanStaleSessions(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("browsedb clean: %w", PathFreeFSError(err))
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
			firstErr = fmt.Errorf("browsedb clean: %w", PathFreeFSError(err))
		}
	}
	return firstErr
}

// sessionIsStale reports whether a directory is old enough and demonstrably
// unlocked. Recent, locked, unreadable, or unsupported cases are not stale.
func sessionIsStale(dir string, now time.Time) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	if now.Sub(info.ModTime()) < staleThreshold {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR, 0) //nolint:gosec // path is an internal session dir joined with a constant lock filename
	if err != nil {
		// An old directory without a lock file predates successful lock creation.
		return errors.Is(err, os.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	ok, supported := tryLock(f)
	if !supported || !ok {
		return false
	}
	unlock(f)
	return true
}
