//go:build !linux && !darwin

package app

// canEnterDir assumes enterable on platforms without access(2) semantics and
// lets the shell launch surface any failure, as it did before the probe
// existed. Extraction (the only producer of unenterable targets) is gated to
// linux/darwin anyway.
func canEnterDir(string) bool { return true }
