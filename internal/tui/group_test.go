package tui

import "testing"

// TestGroupedWindow checks cursor visibility and heading anchoring in flat token indices.
func TestGroupedWindow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cursorPos   int
		hPos        int
		max         int
		n           int
		wantStart   int
		wantEnd     int
		wantPrepend bool
	}{
		{
			name: "contiguous cursor at first data row", cursorPos: 11, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			name: "contiguous cursor near heading", cursorPos: 12, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			name: "contiguous cursor at boundary", cursorPos: 14, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			name: "contiguous end clamps to n", cursorPos: 11, hPos: 10, max: 5, n: 12,
			wantStart: 10, wantEnd: 12, wantPrepend: false,
		},
		{
			name: "non-contiguous deep group", cursorPos: 20, hPos: 10, max: 5, n: 100,
			wantStart: 18, wantEnd: 22, wantPrepend: true,
		},
		{
			name: "non-contiguous at contig boundary", cursorPos: 15, hPos: 10, max: 5, n: 100,
			wantStart: 13, wantEnd: 17, wantPrepend: true,
		},
		{
			// Preserve the smaller centered tail near EOF instead of sliding backward.
			name: "non-contiguous near EOF keeps smaller window", cursorPos: 23, hPos: 10, max: 5, n: 24,
			wantStart: 21, wantEnd: 24, wantPrepend: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, prep := groupedWindow(tc.cursorPos, tc.hPos, tc.max, tc.n)
			if start != tc.wantStart || end != tc.wantEnd || prep != tc.wantPrepend {
				t.Errorf("groupedWindow(cursorPos=%d, hPos=%d, max=%d, n=%d) = (%d, %d, %v), want (%d, %d, %v)",
					tc.cursorPos, tc.hPos, tc.max, tc.n,
					start, end, prep,
					tc.wantStart, tc.wantEnd, tc.wantPrepend)
			}
			if prep {
				if start < tc.hPos+1 {
					t.Errorf("non-contiguous start %d crosses into prior section (hPos=%d)", start, tc.hPos)
				}
			} else if start != tc.hPos {
				t.Errorf("contiguous start %d should equal hPos %d", start, tc.hPos)
			}
			cursorVisible := tc.cursorPos >= start && tc.cursorPos < end
			if !cursorVisible {
				t.Errorf("cursorPos %d not in window [%d, %d)", tc.cursorPos, start, end)
			}
		})
	}
}
