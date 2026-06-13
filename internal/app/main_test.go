package app

import (
	"os"
	"testing"
)

// TestMain isolates TMPDIR for the whole test binary so the temporary password
// files and prompt-tag rcfiles/ZDOTDIRs that ShellSession and applyPromptTag
// write land in a throwaway directory rather than the developer's real /tmp.
// The session tests remove their own scaffolding on the happy path, but a failed
// assertion (a t.Fatalf reached before Cleanup) would otherwise strand a 0600
// resticscope-pw-* (plaintext restic password) or resticscope-bashrc-* file in
// the shared /tmp. The isolated directory is removed when the binary exits
// regardless of which tests pass.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "resticscope-app-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("TMPDIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
