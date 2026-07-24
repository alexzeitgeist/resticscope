package tui

import "time"

// rateWindow is the minimum sample span for recent throughput. Browse and
// extract share it for consistent progress displays.
const rateWindow = 2 * time.Second

// rateSampler computes a windowed rate from a monotonic counter. A sample is
// published only after rateWindow elapses and the counter grows.
type rateSampler struct {
	baseN  int64     // Counter at the start of the current window.
	baseAt time.Time // Start of the current window.
	rate   float64   // Latest rate in units per second.
}

// update records n at now and publishes a new rate only after rateWindow
// elapses and the counter advances.
func (r *rateSampler) update(n int64, now time.Time) {
	if r.baseAt.IsZero() {
		r.baseN = n
		r.baseAt = now
		return
	}
	elapsed := now.Sub(r.baseAt)
	if elapsed < rateWindow || n <= r.baseN {
		return
	}
	r.rate = float64(n-r.baseN) / elapsed.Seconds()
	r.baseN = n
	r.baseAt = now
}
