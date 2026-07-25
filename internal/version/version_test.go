package version

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Version: mainVersion},
		Settings: settings,
	}
}

func revision(sha string) debug.BuildSetting {
	return debug.BuildSetting{Key: keyRevision, Value: sha}
}

func buildTime(stamp string) debug.BuildSetting {
	return debug.BuildSetting{Key: keyTime, Value: stamp}
}

func modified() debug.BuildSetting {
	return debug.BuildSetting{Key: keyModified, Value: "true"}
}

func TestFormat(t *testing.T) {
	const sha = "522ff35b68a1c0de1234567890abcdef12345678"

	tests := []struct {
		name                  string
		version, commit, date string
		info                  *debug.BuildInfo
		want                  string
	}{
		{
			name:    "ldflags win over build info",
			version: "1.2.3", commit: "abc1234", date: "2026-07-25T10:00:00Z",
			info: buildInfo("v9.9.9", revision(sha), buildTime("2020-01-01T00:00:00Z")),
			want: "1.2.3 (commit abc1234, built 2026-07-25T10:00:00Z)",
		},
		{
			name:    "go install of a tagged version reports that version",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: buildInfo("v1.4.0"),
			want: "v1.4.0",
		},
		{
			name:    "build from a clean checkout reports revision and time",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: buildInfo("(devel)", revision(sha), buildTime("2026-07-01T09:30:00Z")),
			want: "dev (commit 522ff35b68a1, built 2026-07-01T09:30:00Z)",
		},
		{
			name:    "build from a modified checkout is marked dirty",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: buildInfo("(devel)", revision(sha), modified()),
			want: "dev (commit 522ff35b68a1+dirty)",
		},
		{
			// The toolchain stamps an untagged build with a pseudo-version that
			// already ends in the revision; repeating it would say nothing new.
			name:    "pseudo-version does not replace dev",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: buildInfo("v0.0.0-20260725092929-522ff35b68a1", revision(sha)),
			want: "dev (commit 522ff35b68a1)",
		},
		{
			name:    "no build info leaves the defaults alone",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: nil,
			want: "dev",
		},
		{
			name:    "short revision is not truncated",
			version: devVersion, commit: noCommit, date: unknownDate,
			info: buildInfo("", revision("abc123")),
			want: "dev (commit abc123)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := format(tt.version, tt.commit, tt.date, tt.info); got != tt.want {
				t.Errorf("format() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The test binary carries build info but no -ldflags, so String exercises the
// fallback end to end.
func TestStringIsNonEmpty(t *testing.T) {
	if got := String(); got == "" {
		t.Error("String() = \"\", want a version summary")
	}
}
