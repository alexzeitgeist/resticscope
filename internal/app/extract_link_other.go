//go:build !linux && !darwin

package app

import "os"

// linkNoFollow falls back to os.Link on platforms without a validated
// linkat(2) binding. Unreachable in practice: App.Extract refuses the whole
// pipeline before any publish when !liveExtractSupported.
func linkNoFollow(oldname, newname string) error {
	return os.Link(oldname, newname)
}
