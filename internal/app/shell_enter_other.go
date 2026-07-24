//go:build !linux && !darwin

package app

// canEnterDir leaves entry validation to shell launch; live extraction is
// disabled on these platforms.
func canEnterDir(string) bool { return true }
