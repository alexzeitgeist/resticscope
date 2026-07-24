package app

import (
	"os"
	"testing"
)

// TestMain isolates TMPDIR so failed shell tests cannot strand password or prompt
// files in shared temporary storage on Unix. The directory is removed after the suite.
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
