package config

import (
	"strings"
	"testing"
)

func TestDiffDefaultsApplied(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Diff.Timeout.Std(); got != defaultDiffTimeout {
		t.Errorf("diff.timeout = %v, want %v", got, defaultDiffTimeout)
	}
}

func TestDiffTimeoutParses(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[diff]
timeout = "15m"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Diff.Timeout.Std().Minutes(); got != 15 {
		t.Errorf("diff.timeout = %v minutes, want 15", got)
	}
}

func TestDiffExplicitZeroTimeoutRejected(t *testing.T) {
	_, err := load(t, minimalTOML+`
[diff]
timeout = "0s"
`)
	if err == nil {
		t.Fatal("expected validation error for explicit diff.timeout = 0, got nil")
	}
	if !strings.Contains(err.Error(), "diff.timeout must be a positive duration") {
		t.Errorf("error = %q, want substring about diff.timeout", err.Error())
	}
}
