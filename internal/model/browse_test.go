package model

import "testing"

// Integration coverage for cleaned directory queries lives in browsedb_test.go.
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

func TestBrowseName(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"f.txt", "/home/f.txt", "f.txt"},
		{"", "/home/alex", "alex"},
		{"", "/", "/"},
		{"", "/etc/passwd", "passwd"},
	} {
		if got := BrowseName(tc.name, tc.path); got != tc.want {
			t.Errorf("BrowseName(%q, %q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}
