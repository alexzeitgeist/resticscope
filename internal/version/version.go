// Package version exposes build metadata, populated at link time via -ldflags.
package version

// Version, Commit, and Date may be overridden at build time with -ldflags -X.
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
