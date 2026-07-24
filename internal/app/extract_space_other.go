//go:build !linux && !darwin

package app

// freeBytesAt reports unknown when no statfs binding is available.
func freeBytesAt(string) (int64, bool) { return 0, false }
