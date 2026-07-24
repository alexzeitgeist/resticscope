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
	if !cfg.Extract.RememberTarget {
		t.Error("remember_target should default to true when the key is omitted")
	}
}

func TestExtractRememberTargetHonorsExplicitFalse(t *testing.T) {
	cfg, err := load(t, minimalTOML+"\n[extract]\nremember_target = false\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Extract.RememberTarget {
		t.Error("explicit remember_target = false must be honored, got true")
	}
}

// UnsafeSymlinks accepts keep, skip, and placeholder. Validation errors identify
// the key and allowed values without echoing the input.
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

// Invalid target roots are reported without echoing the user-supplied path.
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

// An explicit empty root overrides the seeded default and must fail validation.
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

// Explicit non-positive timeouts override the seeded default and must fail validation.
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
