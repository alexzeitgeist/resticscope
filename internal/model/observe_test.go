package model

import (
	"reflect"
	"testing"
	"time"
)

func TestObserved(t *testing.T) {
	snaps := []Snapshot{
		{Hostname: "homeserver", Paths: []string{"/etc", "/var/lib"}, Tags: []string{"daily", "system"}},
		{Hostname: "homeserver", Paths: []string{"/etc"}, Tags: []string{"daily"}},
		{Hostname: "laptop", Paths: []string{"/home"}, Tags: nil},
	}
	hosts, paths, tags := Observed(snaps)
	if want := []string{"homeserver", "laptop"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	if want := []string{"/etc", "/home", "/var/lib"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	if want := []string{"daily", "system"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestObservedEmpty(t *testing.T) {
	hosts, paths, tags := Observed(nil)
	if hosts != nil || paths != nil || tags != nil {
		t.Errorf("expected nil slices, got %v %v %v", hosts, paths, tags)
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
