package model

import (
	"reflect"
	"testing"
	"time"
)

func TestComputeCoverage(t *testing.T) {
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

	t.Run("fully covered", func(t *testing.T) {
		exp := Expectation{
			Hosts:     []string{"homeserver"},
			Paths:     []string{"/etc", "/var/lib"},
			Tags:      []string{"daily"},
			Frequency: 24 * time.Hour,
		}
		state := RepoState{
			Hosts:        []string{"homeserver"},
			Paths:        []string{"/etc", "/var/lib", "/home"},
			Tags:         []string{"daily", "system"},
			LastSnapshot: now.Add(-2 * time.Hour),
		}
		got := ComputeCoverage(now, exp, state)
		if !got.Covered() {
			t.Fatalf("expected covered, got %+v", got)
		}
	})

	t.Run("reports each kind of gap", func(t *testing.T) {
		exp := Expectation{
			Hosts:     []string{"homeserver", "backup-host"},
			Paths:     []string{"/etc", "/srv"},
			Tags:      []string{"daily", "weekly"},
			Frequency: 24 * time.Hour,
		}
		state := RepoState{
			Hosts:        []string{"homeserver"},
			Paths:        []string{"/etc"},
			Tags:         []string{"daily"},
			LastSnapshot: now.Add(-48 * time.Hour), // stale
		}
		got := ComputeCoverage(now, exp, state)

		if want := []string{"backup-host"}; !reflect.DeepEqual(got.MissingHosts, want) {
			t.Errorf("MissingHosts = %v, want %v", got.MissingHosts, want)
		}
		if want := []string{"/srv"}; !reflect.DeepEqual(got.MissingPaths, want) {
			t.Errorf("MissingPaths = %v, want %v", got.MissingPaths, want)
		}
		if want := []string{"weekly"}; !reflect.DeepEqual(got.MissingTags, want) {
			t.Errorf("MissingTags = %v, want %v", got.MissingTags, want)
		}
		if !got.Stale {
			t.Error("expected Stale = true")
		}
		if got.Covered() {
			t.Error("expected not covered")
		}
	})

	t.Run("no expectations means covered with no gaps", func(t *testing.T) {
		got := ComputeCoverage(now, Expectation{}, RepoState{})
		if !got.Covered() {
			t.Errorf("expected covered, got %+v", got)
		}
		if got.MissingHosts != nil || got.MissingPaths != nil || got.MissingTags != nil {
			t.Errorf("expected nil missing slices, got %+v", got)
		}
	})

	t.Run("frequency set but no snapshot is stale", func(t *testing.T) {
		got := ComputeCoverage(now, Expectation{Frequency: time.Hour}, RepoState{})
		if !got.Stale {
			t.Error("expected stale when no snapshot and frequency set")
		}
	})
}
