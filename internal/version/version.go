// Package version exposes build metadata, populated at link time via -ldflags.
package version

import (
	"runtime/debug"
	"strings"
	"sync"
)

// Sentinels for a field that -ldflags did not set, and the documented defaults
// of the exported variables below.
const (
	devVersion  = "dev"
	noCommit    = "none"
	unknownDate = "unknown"
)

// Keys of the build settings the Go toolchain stamps into a binary built from
// a VCS checkout.
const (
	keyRevision = "vcs.revision"
	keyTime     = "vcs.time"
	keyModified = "vcs.modified"
)

// shortSHA is the width the fallback abbreviates an embedded revision to: long
// enough to stay unambiguous, short enough to read in a terminal.
const shortSHA = 12

// Version, Commit, and Date may be overridden at build time with -ldflags -X.
var (
	// Version is the semantic version of the build, or "dev" for local builds.
	Version = devVersion
	// Commit is the short git SHA of the build.
	Commit = noCommit
	// Date is the RFC3339 build timestamp.
	Date = unknownDate
)

// String returns a human-readable one-line version summary.
func String() string { return describe() }

// Released binaries carry all three fields from -ldflags. Binaries from
// `go install module@version` or a plain `go build` carry none, so fall back to
// what the toolchain embeds; otherwise every such binary calls itself
// "dev (commit none, built unknown)", which tells a bug report nothing.
var describe = sync.OnceValue(func() string {
	info, _ := debug.ReadBuildInfo()
	return format(Version, Commit, Date, info)
})

// format renders the summary, replacing any field still at its default with
// embedded build metadata from info, which may be nil. Fields that stay unknown
// are left out rather than reported as "none", and a build with uncommitted
// changes is marked, since its revision alone misdescribes it.
func format(version, commit, date string, info *debug.BuildInfo) string {
	if info != nil {
		revision, dirty := "", false
		for _, s := range info.Settings {
			switch s.Key {
			case keyRevision:
				revision = s.Value[:min(len(s.Value), shortSHA)]
			case keyTime:
				if date == unknownDate {
					date = s.Value
				}
			case keyModified:
				dirty = s.Value == "true"
			}
		}
		if commit == noCommit && revision != "" {
			commit = revision
		}
		if dirty && commit != noCommit {
			commit += "+dirty"
		}
		if version == devVersion && isRelease(info.Main.Version, revision) {
			version = info.Main.Version
		}
	}

	var details []string
	if commit != noCommit {
		details = append(details, "commit "+commit)
	}
	if date != unknownDate {
		details = append(details, "built "+date)
	}
	if len(details) == 0 {
		return version
	}
	return version + " (" + strings.Join(details, ", ") + ")"
}

// isRelease reports whether a main module version is worth showing in place of
// "dev". A pseudo-version such as v0.0.0-20260725092929-522ff35b68a1 embeds the
// revision, so it would only repeat what the commit field already carries.
func isRelease(moduleVersion, revision string) bool {
	if moduleVersion == "" || moduleVersion == "(devel)" {
		return false
	}
	return revision == "" || !strings.Contains(moduleVersion, revision)
}
