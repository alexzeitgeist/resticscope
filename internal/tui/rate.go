package tui

import "time"

// rateWindow is the minimum sample span for a displayed recent-throughput rate.
// Browse (entries/sec) and extract (bytes/sec) share it so both feel the same
// to the user.
const rateWindow = 2 * time.Second

// rateSampler computes a windowed recent rate from a monotonically growing
// counter: the first sample seeds the window, and the rate advances only when
// at least rateWindow has elapsed AND the counter actually grew. Shared by the
// browse indexer's progress line and the extract running view.
type rateSampler struct {
	baseN  int64     // count at the start of the current rate window
	baseAt time.Time // timestamp at the start of the current rate window
	rate   float64   // most recent windowed rate, in units/sec
}

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
