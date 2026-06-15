package tui

import "testing"

func TestScrollWindowFloorsVisible(t *testing.T) {
	for _, visible := range []int{0, -3} {
		start, end := scrollWindow(5, 10, visible)
		if start != 5 || end != 6 {
			t.Errorf("scrollWindow visible=%d = (%d, %d), want (5, 6)", visible, start, end)
		}
	}
}

func TestDetailSnapshotsMissingRowReturnsNil(t *testing.T) {
	if snaps := (Model{}).detailSnapshots(); snaps != nil {
		t.Errorf("detailSnapshots without a detail row = %#v, want nil", snaps)
	}
}
