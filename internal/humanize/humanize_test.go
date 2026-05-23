package humanize

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
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
