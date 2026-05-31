package tui

import "testing"

// TestGroupedWindow exercises the pure window-bounds math extracted from
// renderGroupedList. Inputs are flat token-stream indexes; the function must
// keep the cursor visible AND anchor its section heading without ever crossing
// into a prior section.
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
			// Cursor on the first data row of the section (one past the
			// heading): the window starts at the heading, ends max lines
			// later. This is the smallest reachable cursorPos because the
			// caller only sets cursorPos to a data-row index, never to hPos.
			name: "contiguous cursor at first data row", cursorPos: 11, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			// Cursor a few rows under the heading: still contiguous, window
			// still anchors at hPos.
			name: "contiguous cursor near heading", cursorPos: 12, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			// Cursor at the last position that still allows contiguous render
			// (cursorPos == hPos+max-1). Boundary case for the < check.
			name: "contiguous cursor at boundary", cursorPos: 14, hPos: 10, max: 5, n: 100,
			wantStart: 10, wantEnd: 15, wantPrepend: false,
		},
		{
			// Contiguous window runs off the end of the token stream; end
			// clamps to n. The cursor is still inside [start, end).
			name: "contiguous end clamps to n", cursorPos: 11, hPos: 10, max: 5, n: 12,
			wantStart: 10, wantEnd: 12, wantPrepend: false,
		},
		{
			// Cursor too far below the heading for a contiguous window. Tail
			// of size max-1 centers on the cursor; heading prepends.
			name: "non-contiguous deep group", cursorPos: 20, hPos: 10, max: 5, n: 100,
			wantStart: 18, wantEnd: 22, wantPrepend: true,
		},
		{
			// Boundary into non-contiguous: cursorPos exactly at hPos+max.
			// Centered tail naturally lands well past hPos+1 — the "never
			// cross the heading" invariant holds without the clamp firing.
			name: "non-contiguous at contig boundary", cursorPos: 15, hPos: 10, max: 5, n: 100,
			wantStart: 13, wantEnd: 17, wantPrepend: true,
		},
		{
			// Near-EOF non-contiguous: cursor sits in the last few rows so the
			// centered tail's end exceeds n. We pin the CURRENT behavior —
			// end clamps to n and start is NOT slid back to refill the tail.
			// The visible window is intentionally smaller than max-1. If a
			// future patch decides to slide back to fill the budget, this
			// test should be updated deliberately.
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
			// Heading-anchoring invariant: the rendered range never includes
			// content above the heading.
			if prep {
				if start < tc.hPos+1 {
					t.Errorf("non-contiguous start %d crosses into prior section (hPos=%d)", start, tc.hPos)
				}
			} else if start != tc.hPos {
				t.Errorf("contiguous start %d should equal hPos %d", start, tc.hPos)
			}
			// Cursor must always be visible (either prepended heading line or
			// inside [start, end)).
			cursorVisible := tc.cursorPos >= start && tc.cursorPos < end
			if !cursorVisible {
				t.Errorf("cursorPos %d not in window [%d, %d)", tc.cursorPos, start, end)
			}
		})
	}
}
