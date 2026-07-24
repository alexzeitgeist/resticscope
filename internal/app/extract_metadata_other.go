//go:build !linux && !darwin

package app

import (
	"context"
	"fmt"
)

// liveExtractSupported is false where metadata normalization and single-file
// include escaping are unvalidated, causing refusal before staging or restic.
var liveExtractSupported = false

func normalizeExtractTreeMetadata(_ context.Context, _ string, _ unsafeSymlinkPolicy) (extractMetaCounts, error) {
	return extractMetaCounts{}, fmt.Errorf("%w: not supported on this platform", ErrExtractMetadataNormalization)
}
