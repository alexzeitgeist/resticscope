package humanize

import (
	"math"
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{-1, "—"},
		{0, "0 B"},
		{512, "512 B"},
		{442000000000, "412 GiB"},
		{4400000000, "4.1 GiB"},
		{18000000000, "17 GiB"},
		{72 * 1024 * 1024 * 1024, "72 GiB"},
	}
	for _, tt := range tests {
		if got := Bytes(tt.n); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "<1s"},
		{-5 * time.Second, "—"},
		{600 * time.Millisecond, "<1s"},
		{28 * time.Second, "28s"},
		{4*time.Minute + 12*time.Second, "4m12s"},
		{4 * time.Minute, "4m00s"},
		{time.Hour + 3*time.Minute, "1h03m"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, tt := range tests {
		if got := Duration(tt.d); got != tt.want {
			t.Errorf("Duration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestCount(t *testing.T) {
	tests := []struct {
		n                int
		singular, plural string
		want             string
	}{
		{0, "file", "files", "0 files"},
		{1, "file", "files", "1 file"},
		{2, "file", "files", "2 files"},
		{1, "entry", "entries", "1 entry"},
		{3, "entry", "entries", "3 entries"},
	}
	for _, tt := range tests {
		if got := Count(tt.n, tt.singular, tt.plural); got != tt.want {
			t.Errorf("Count(%d, %q, %q) = %q, want %q", tt.n, tt.singular, tt.plural, got, tt.want)
		}
	}

	// The generic signature carries uint64 counters at full precision — no
	// narrowing through int on the way to the format verb.
	if got, want := Count(uint64(math.MaxUint64), "file", "files"), "18446744073709551615 files"; got != want {
		t.Errorf("Count(MaxUint64) = %q, want %q", got, want)
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "never"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-35 * time.Minute), "35m ago"},
		{now.Add(-8 * time.Hour), "8h ago"},
		{now.Add(-9 * 24 * time.Hour), "9d ago"},
		{now.Add(time.Hour), "just now"}, // future clamps
	}
	for _, tt := range tests {
		if got := Ago(now, tt.t); got != tt.want {
			t.Errorf("Ago(%v) = %q, want %q", tt.t, got, tt.want)
		}
	}
}
