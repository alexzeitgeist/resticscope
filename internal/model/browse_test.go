package model

import "testing"

// CleanBrowsePath is the one shared path rule resticx-stream, browsedb, and the
// TUI agree on: a rooted, lexically clean key. Directory listing/ordering and the
// against-real-queries path-clean cases live in browsedb_test.go; this asserts the
// pure rule directly.
func TestCleanBrowsePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/"},
		{".", "/"},
		{"/", "/"},
		{"//", "/"},
		{"/etc/", "/etc"},
		{"etc/passwd", "/etc/passwd"},
		{"/a/../b", "/b"},
		{"/home/alex/photos", "/home/alex/photos"},
	} {
		if got := CleanBrowsePath(tc.in); got != tc.want {
			t.Errorf("CleanBrowsePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// BrowseName prefers the emitted name and falls back to the base of the cleaned
// path, with the root shown as "/".
func TestBrowseName(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"f.txt", "/home/f.txt", "f.txt"}, // emitted name wins
		{"", "/home/alex", "alex"},        // fall back to base
		{"", "/", "/"},                    // root
		{"", "/etc/passwd", "passwd"},
	} {
		if got := BrowseName(tc.name, tc.path); got != tc.want {
			t.Errorf("BrowseName(%q, %q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}
