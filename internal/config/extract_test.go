package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractDefaultsApplied(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantRoot := filepath.Join("/home/tester", "resticscope-extracts")
	if cfg.Extract.TargetRoot != wantRoot {
		t.Errorf("target_root = %q, want %q", cfg.Extract.TargetRoot, wantRoot)
	}
	if got := cfg.Extract.ExtractTimeout.Std(); got != 30*time.Minute {
		t.Errorf("extract_timeout = %v, want 30m", got)
	}
	if cfg.Extract.UnsafeSymlinks != "keep" {
		t.Errorf("unsafe_symlinks = %q, want default %q", cfg.Extract.UnsafeSymlinks, "keep")
	}
}

// unsafe_symlinks is enum-validated: skip/placeholder are accepted, anything else
// is rejected. The error names the key and the allowed set but never echoes the
// bad value.
func TestExtractUnsafeSymlinksEnum(t *testing.T) {
	for _, ok := range []string{"keep", "skip", "placeholder"} {
		cfg, err := load(t, minimalTOML+"\n[extract]\nunsafe_symlinks = \""+ok+"\"\n")
		if err != nil {
			t.Errorf("unsafe_symlinks = %q rejected: %v", ok, err)
			continue
		}
		if cfg.Extract.UnsafeSymlinks != ok {
			t.Errorf("unsafe_symlinks = %q, want %q", cfg.Extract.UnsafeSymlinks, ok)
		}
	}

	_, err := load(t, minimalTOML+"\n[extract]\nunsafe_symlinks = \"bogus\"\n")
	if err == nil {
		t.Fatal("expected a validation error for unsafe_symlinks = \"bogus\", got nil")
	}
	if !strings.Contains(err.Error(), "extract.unsafe_symlinks") {
		t.Errorf("error = %q, want substring %q", err.Error(), "extract.unsafe_symlinks")
	}
	if strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q leaked the user-supplied value", err.Error())
	}
}

// A leading ~ is expanded against the home passed to Normalize, and the result
// is absolute by the time Validate runs.
func TestExtractTargetRootExpands(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[extract]
target_root = "~/elsewhere"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/home/tester", "elsewhere")
	if cfg.Extract.TargetRoot != want {
		t.Errorf("target_root = %q, want %q", cfg.Extract.TargetRoot, want)
	}
	if !filepath.IsAbs(cfg.Extract.TargetRoot) {
		t.Errorf("target_root = %q, want absolute", cfg.Extract.TargetRoot)
	}
}

func TestExtractTimeoutParses(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[extract]
extract_timeout = "45m"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Extract.ExtractTimeout.Std().Minutes(); got != 45 {
		t.Errorf("extract_timeout = %v minutes, want 45", got)
	}
}

// A relative target_root is rejected by Validate. The error names the key but
// must never echo the user-supplied path — the path-free discipline that governs
// extract source/destination paths starts at config time.
func TestExtractTargetRootMustBeAbsolute(t *testing.T) {
	_, err := load(t, minimalTOML+`
[extract]
target_root = "relative/path"
`)
	if err == nil {
		t.Fatal("expected validation error for relative target_root, got nil")
	}
	if !strings.Contains(err.Error(), "target_root") {
		t.Errorf("error = %q, want substring %q", err.Error(), "target_root")
	}
	if strings.Contains(err.Error(), "relative/path") {
		t.Errorf("error %q leaked the user-supplied path", err.Error())
	}
}

// An explicit empty target_root is rejected, not silently defaulted. target_root
// is seeded with the default in Decode, so an omitted key keeps the default while
// an explicit "" overwrites it and survives to validation — distinguishable from
// omitted, exactly like an explicit-zero timeout.
func TestExtractEmptyTargetRootRejected(t *testing.T) {
	_, err := load(t, minimalTOML+`
[extract]
target_root = ""
`)
	if err == nil {
		t.Fatal("expected validation error for empty target_root, got nil")
	}
	if !strings.Contains(err.Error(), "extract.target_root") {
		t.Errorf("error = %q, want substring %q", err.Error(), "extract.target_root")
	}
}

// extract_timeout is seeded with its default before decode, so an explicit 0 or
// negative value survives into validation and is rejected rather than silently
// defaulted — the same discipline as browse.index_timeout / diff.timeout.
func TestExtractExplicitNonPositiveTimeoutRejected(t *testing.T) {
	for _, bad := range []string{`"0s"`, `"-1m"`} {
		_, err := load(t, minimalTOML+"\n[extract]\nextract_timeout = "+bad+"\n")
		if err == nil {
			t.Fatalf("expected validation error for extract_timeout = %s, got nil", bad)
		}
		if !strings.Contains(err.Error(), "extract.extract_timeout must be a positive duration") {
			t.Errorf("error = %q, want substring about extract_timeout", err.Error())
		}
	}
}

func TestExtractRejectsUnknownKey(t *testing.T) {
	_, err := load(t, minimalTOML+`
[extract]
target_root = "~/elsewhere"
bogus = 5
`)
	if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
		t.Fatalf("expected unknown-keys error, got %v", err)
	}
}
