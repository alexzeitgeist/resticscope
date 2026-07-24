package resticx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MinVersion is the oldest supported restic release. Version 0.17.0 introduced
// exit codes 10 and 11; its JSON output is this package's compatibility baseline.
const MinVersion = "0.17.0"

type version struct{ major, minor, patch int }

// AtLeastMinVersion reports whether s is at least MinVersion. Malformed versions
// return an error rather than being assumed supported.
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

// parseVersion ignores pre-release and build suffixes; absent minor or patch
// components default to zero.
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
