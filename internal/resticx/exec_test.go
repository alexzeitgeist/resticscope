//go:build linux || darwin

package resticx

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecRunnerDeliversPatternsOnFD4 is the real-process integration check for
// the fd-4 pattern delivery: a PATH-shimmed fake "restic" echoes back what it
// reads from /dev/fd/3 and /dev/fd/4, proving both payloads arrive on their
// fixed slots. The pattern payload is deliberately larger than the default
// 64 KiB kernel pipe buffer, so the test also proves the async writer cannot
// deadlock the spawn (a synchronous pre-spawn write would hang here).
func TestExecRunnerDeliversPatternsOnFD4(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf 'PW:'; cat /dev/fd/3\n" +
		"printf '\\nPATTERNS:'; cat /dev/fd/4\n"
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	patterns := bytes.Repeat([]byte("/some/include/pattern/line\n"), 5000) // ~130 KiB > pipe buffer

	var got []byte
	stderr, err := ExecRunner{}.RunStreamPatterns(t.Context(),
		[]string{"PATH=" + os.Getenv("PATH")}, "secret-pw", patterns,
		func(r io.Reader) error {
			b, readErr := io.ReadAll(r)
			got = b
			return readErr
		})
	if err != nil {
		t.Fatalf("RunStreamPatterns: %v (stderr %q)", err, stderr)
	}
	out := string(got)
	pwPart, patPart, found := strings.Cut(out, "\nPATTERNS:")
	if !found {
		t.Fatalf("output missing PATTERNS marker: %q…", out[:min(len(out), 120)])
	}
	if pwPart != "PW:secret-pw" {
		t.Errorf("fd-3 password readback = %q, want %q", pwPart, "PW:secret-pw")
	}
	if patPart != string(patterns) {
		t.Errorf("fd-4 payload mismatch: got %d bytes, want %d (first divergence near %d)",
			len(patPart), len(patterns), firstDiff(patPart, string(patterns)))
	}
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
