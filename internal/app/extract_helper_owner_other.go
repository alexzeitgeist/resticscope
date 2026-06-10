//go:build !linux && !darwin

package app

import "io/fs"

// ownedByUID cannot be answered without unix stat semantics; refusing keeps
// the shared cache off, matching liveExtractSupported (the helper never runs
// a real extract on these platforms anyway).
func ownedByUID(fs.FileInfo, int) bool { return false }
