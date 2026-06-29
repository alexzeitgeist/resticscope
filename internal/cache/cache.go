// Package cache persists one model.RepoState JSON file per repository under the
// cache directory. It is the only state resticscope writes to disk.
//
// Two invariants matter most: writes are atomic (write a temp file, then
// rename), and the files never contain credentials. The second holds by design —
// RepoState carries no secret fields — but the tests assert it anyway.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"resticscope/internal/model"
)

// ErrMiss means there is no usable cached state for a repo: either no file
// exists or it could not be read. ErrCorrupt additionally signals that a file
// was present but unparseable; it wraps ErrMiss, so a single errors.Is check
// for ErrMiss covers both, while callers that want to warn can test ErrCorrupt.
var (
	ErrMiss     = errors.New("cache miss")
	ErrCorrupt  = fmt.Errorf("cache file corrupt: %w", ErrMiss)
	errNilStore = errors.New("cache: nil Store")
)

// Store reads and writes per-repo cache files under Dir.
type Store struct {
	dir string
}

// New returns a Store rooted at dir. The directory is created on first Save.
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, sanitize(name)+".json")
}

// Load reads the cached state for name. A missing file returns ErrMiss; a
// present-but-unparseable file returns ErrCorrupt (which is also ErrMiss).
func (s *Store) Load(ctx context.Context, name string) (model.RepoState, error) {
	if s == nil {
		return model.RepoState{}, errNilStore
	}
	if err := ctx.Err(); err != nil {
		return model.RepoState{}, err
	}
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.RepoState{}, ErrMiss
		}
		return model.RepoState{}, fmt.Errorf("read cache %q: %w", name, errors.Join(err, ErrMiss))
	}
	var state model.RepoState
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt cache is treated as cold, not fatal.
		return model.RepoState{}, fmt.Errorf("decode cache %q: %w: %w", name, err, ErrCorrupt)
	}
	return state, nil
}

// Save atomically writes state for name: marshal, write a temp file in the same
// directory, fsync, then rename over the target.
func (s *Store) Save(ctx context.Context, name string, state model.RepoState) error {
	if s == nil {
		return errNilStore
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache %q: %w", name, err)
	}

	tmp, err := os.CreateTemp(s.dir, sanitize(name)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed; cleans up on any error path

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp cache file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cache file: %w", err)
	}
	if err := os.Rename(tmpName, s.path(name)); err != nil {
		return fmt.Errorf("rename cache file: %w", err)
	}
	return nil
}

// sanitize makes a repo name safe to use as a single path element.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
}
