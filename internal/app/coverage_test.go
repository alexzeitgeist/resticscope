package app

import (
	"testing"

	"resticscope/internal/model"
)

func TestRollupCountsCoveredAndCollectsGapsInOrder(t *testing.T) {
	rows := []RepoStatus{
		{Name: "a", Coverage: model.Coverage{}},                                 // covered
		{Name: "b", Coverage: model.Coverage{Stale: true}},                      // gap: stale
		{Name: "c", Coverage: model.Coverage{MissingHosts: []string{"laptop"}}}, // gap: host
		{Name: "d", Coverage: model.Coverage{}},                                 // covered
	}
	r := Rollup(rows)

	if r.Total != 4 {
		t.Errorf("Total = %d, want 4", r.Total)
	}
	if r.Covered != 2 {
		t.Errorf("Covered = %d, want 2", r.Covered)
	}
	if r.FullyCovered() {
		t.Error("FullyCovered = true, want false with gaps present")
	}
	if len(r.Gaps) != 2 {
		t.Fatalf("Gaps = %d, want 2", len(r.Gaps))
	}
	if r.Gaps[0].Repo != "b" || r.Gaps[1].Repo != "c" {
		t.Errorf("gap order = %q,%q, want b,c (config order preserved)", r.Gaps[0].Repo, r.Gaps[1].Repo)
	}
	if got := r.Gaps[0].Coverage.Summary(); got != "stale" {
		t.Errorf("first gap summary = %q, want %q", got, "stale")
	}
}

func TestRollupAllCovered(t *testing.T) {
	r := Rollup([]RepoStatus{{Name: "a"}, {Name: "b"}})
	if !r.FullyCovered() || r.Covered != 2 || len(r.Gaps) != 0 {
		t.Errorf("expected every repo covered, got %+v", r)
	}
}

func TestRollupEmpty(t *testing.T) {
	r := Rollup(nil)
	if !r.FullyCovered() || r.Total != 0 || r.Covered != 0 {
		t.Errorf("empty rollup = %+v, want zero/fully-covered", r)
	}
}
