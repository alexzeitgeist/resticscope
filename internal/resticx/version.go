package resticx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MinVersion is the oldest restic resticscope supports. 0.17.0 is where restic
// introduced the stable exit codes this package classifies (10 repository does
// not exist, 11 failed to lock, 12 wrong password) and the consistent --json
// output the parsers depend on. Refusing to run against anything older is
// safer than misreading its output (plan §9, §12; recommendation 6).
const MinVersion = "0.17.0"

// version is a parsed restic version, compared field by field.
type version struct{ major, minor, patch int }

// AtLeastMinVersion reports whether a restic version string (e.g. "0.18.1", as
// returned by Client.Version) is at least MinVersion. A string it cannot parse
// is returned as an error rather than a silent pass: an unknown restic is
// treated as unsupported, not assumed new enough.
func AtLeastMinVersion(s string) (bool, error) {
	got, err := parseVersion(s)
	if err != nil {
		return false, err
	}
	min, err := parseVersion(MinVersion)
	if err != nil {
		// MinVersion is a constant; a parse failure here is a programmer error.
		return false, fmt.Errorf("internal: bad MinVersion %q: %w", MinVersion, err)
	}
	return !got.less(min), nil
}

// parseVersion parses a restic version like "0.18.1" or "0.18.1-dev" into its
// numeric components. A pre-release/build suffix introduced by '-' or '+' is
// ignored; an absent minor or patch defaults to 0.
func parseVersion(s string) (version, error) {
	core := strings.TrimSpace(s)
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	if core == "" {
		return version{}, errors.New("empty version string")
	}
	parts := strings.Split(core, ".")
	if len(parts) > 3 {
		return version{}, fmt.Errorf("malformed version %q", s)
	}
	var v version
	dst := []*int{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, fmt.Errorf("malformed version %q", s)
		}
		*dst[i] = n
	}
	return v, nil
}

func (v version) less(o version) bool {
	switch {
	case v.major != o.major:
		return v.major < o.major
	case v.minor != o.minor:
		return v.minor < o.minor
	default:
		return v.patch < o.patch
	}
}
