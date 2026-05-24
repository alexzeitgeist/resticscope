package model

import (
	"reflect"
	"testing"
	"time"
)

func TestObserved(t *testing.T) {
	snaps := []Snapshot{
		{Hostname: "homeserver", Tags: []string{"daily", "system"}},
		{Hostname: "homeserver", Tags: []string{"daily"}},
		{Hostname: "laptop", Tags: nil},
	}
	hosts, tags := Observed(snaps)
	if want := []string{"homeserver", "laptop"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	if want := []string{"daily", "system"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestObservedFiltersEmptyTagsAndHosts(t *testing.T) {
	snaps := []Snapshot{
		{Hostname: "", Tags: []string{"", "daily"}},
		{Hostname: "laptop", Tags: []string{""}},
	}
	hosts, tags := Observed(snaps)
	if want := []string{"laptop"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v (empty hostname must be dropped)", hosts, want)
	}
	if want := []string{"daily"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v (empty tag must be dropped)", tags, want)
	}
}

func TestObservedEmpty(t *testing.T) {
	hosts, tags := Observed(nil)
	if hosts != nil || tags != nil {
		t.Errorf("expected nil slices, got %v %v", hosts, tags)
	}
}

func TestLatestSnapshotTime(t *testing.T) {
	t1 := time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 23, 6, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 22, 6, 0, 0, 0, time.UTC)
	got := LatestSnapshotTime([]Snapshot{{Time: t1}, {Time: t2}, {Time: t3}})
	if !got.Equal(t2) {
		t.Errorf("latest = %v, want %v", got, t2)
	}
	if z := LatestSnapshotTime(nil); !z.IsZero() {
		t.Errorf("empty latest = %v, want zero", z)
	}
}
