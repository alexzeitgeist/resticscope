//go:build !linux && !darwin

package app

import "os"

// linkNoFollow falls back to os.Link; live extraction rejects these platforms
// before publish.
func linkNoFollow(oldname, newname string) error {
	return os.Link(oldname, newname)
}
