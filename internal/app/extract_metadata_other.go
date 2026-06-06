//go:build !linux && !darwin

package app

// extract_metadata_other.go is the fallback for platforms without the normalizer.
// The pass needs Lchown / l*xattr semantics (unix.Llistxattr, unix.Lremovexattr,
// unix.ENODATA) that are portable only on linux and darwin — other unix targets
// (freebsd, openbsd) lack those symbols, and Windows lacks the model entirely —
// so on any non-(linux|darwin) platform a live tree extract refuses rather than
// publishing un-normalized restore metadata.

import (
	"context"
	"fmt"
	"time"
)

// extractMetaCounts mirrors the unix struct's Files/Dirs fields used by the
// orchestrator.
type extractMetaCounts struct {
	Files int
	Dirs  int
}

func normalizeExtractTreeMetadata(_ context.Context, _ string, _ time.Time) (extractMetaCounts, error) {
	return extractMetaCounts{}, fmt.Errorf("%w: not supported on this platform", ErrExtractMetadataNormalization)
}
