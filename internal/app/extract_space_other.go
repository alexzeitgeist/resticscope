//go:build !linux && !darwin

package app

// freeBytesAt reports no answer on platforms without a statfs binding; the
// review screen's space preflight simply stays silent there.
func freeBytesAt(string) (int64, bool) { return 0, false }
