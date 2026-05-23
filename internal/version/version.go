// Package version exposes build metadata, populated at link time via -ldflags.
package version

// These are overridden at build time, e.g.:
//
//	go build -ldflags "-X resticscope/internal/version.Version=v0.1.0 \
//	    -X resticscope/internal/version.Commit=$(git rev-parse --short HEAD) \
//	    -X resticscope/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
var (
	// Version is the semantic version of the build, or "dev" for local builds.
	Version = "dev"
	// Commit is the short git SHA of the build.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// String returns a human-readable one-line version summary.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
