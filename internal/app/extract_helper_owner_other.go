//go:build !linux && !darwin

package app

import "io/fs"

// ownedByUID refuses proof without Unix stat semantics, disabling shared cache
// on unsupported platforms.
func ownedByUID(fs.FileInfo, int) bool { return false }
