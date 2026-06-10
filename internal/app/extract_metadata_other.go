//go:build !linux && !darwin

package app

// extract_metadata_other.go is the fallback for platforms the normalizer is not
// validated on. The post-restore staging pass — its mode handling, symlink
// classification, and the directory temp-chmod-and-restore traversal trick — is
// exercised and tested only on linux and darwin; Windows mode/symlink semantics
// in particular are untested and the dir-chmod trick is meaningless there. So on
// any non-(linux|darwin) platform a live tree extract refuses rather than
// publishing restore metadata that has not been verified to round-trip.
//
// unsafeSymlinkPolicy and extractMetaCounts live in extract.go so this stub and
// the unix implementation share one definition.

import (
	"context"
	"fmt"
)

// liveExtractSupported is false on platforms the normalizer is not validated on:
// App.Extract refuses a live extract here before any restic spawn or staging,
// because the post-restore normalizer is untested and the single-file --include
// escaping is not Windows-safe. Its sibling in extract_metadata.go is true.
var liveExtractSupported = false

func normalizeExtractTreeMetadata(_ context.Context, _ string, _ unsafeSymlinkPolicy) (extractMetaCounts, error) {
	return extractMetaCounts{}, fmt.Errorf("%w: not supported on this platform", ErrExtractMetadataNormalization)
}
