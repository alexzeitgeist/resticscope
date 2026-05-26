package config

import (
	"strings"
	"testing"
)

func TestBrowseDefaultsApplied(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := cfg.Browse
	if b.IndexTimeout.Std() != defaultBrowseIndexTimeout {
		t.Errorf("index_timeout = %v, want %v", b.IndexTimeout.Std(), defaultBrowseIndexTimeout)
	}
	if b.MaxDiskBytes.Bytes() != defaultBrowseMaxDiskBytes {
		t.Errorf("max_disk_bytes = %d, want %d", b.MaxDiskBytes.Bytes(), defaultBrowseMaxDiskBytes)
	}
}

func TestBrowseIndexTimeoutParses(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[browse]
index_timeout = "5m"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Browse.IndexTimeout.Std().Minutes(); got != 5 {
		t.Errorf("index_timeout = %v minutes, want 5", got)
	}
}

func TestBrowseMaxDiskBytesParses(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[browse]
max_disk_bytes = "2GiB"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Browse.MaxDiskBytes.Bytes(); got != 2<<30 {
		t.Errorf("max_disk_bytes = %d, want %d", got, 2<<30)
	}
}

func TestBrowseValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		browse  string
		wantSub string
	}{
		{
			name:    "negative timeout survives normalize then fails validate",
			browse:  `index_timeout = "-5s"`,
			wantSub: "browse.index_timeout must be a positive duration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, minimalTOML+"\n[browse]\n"+tt.browse+"\n")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// Invalid and negative byte strings are rejected at Decode, before Normalize can
// turn a zero into a default — the parser refuses them outright.
func TestBrowseRejectsBadByteSizeAtDecode(t *testing.T) {
	for _, bad := range []string{`"nonsense"`, `"-4MiB"`} {
		_, err := Decode([]byte(minimalTOML + "\n[browse]\nmax_disk_bytes = " + bad + "\n"))
		if err == nil {
			t.Errorf("expected decode error for max_disk_bytes = %s, got nil", bad)
		}
	}
}

func TestBrowseRejectsUnknownKey(t *testing.T) {
	_, err := load(t, minimalTOML+`
[browse]
index_timeout = "5m"
bogus = 5
`)
	if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
		t.Fatalf("expected unknown-keys error, got %v", err)
	}
}

// An explicit zero index_timeout is not treated like an omitted value: it is
// seeded with the default before decode, so a configured `0` overwrites the
// default and survives into validation, where it is rejected — letting a typo'd
// or deliberately-zeroed timeout fail loudly instead of silently defaulting.
func TestBrowseExplicitZeroTimeoutRejected(t *testing.T) {
	_, err := load(t, minimalTOML+`
[browse]
index_timeout = "0s"
`)
	if err == nil {
		t.Fatal("expected validation error for explicit index_timeout = 0, got nil")
	}
	if !strings.Contains(err.Error(), "browse.index_timeout must be a positive duration") {
		t.Errorf("error = %q, want substring about index_timeout", err.Error())
	}
}

// max_disk_bytes = 0 means unlimited and must be accepted, not rejected.
func TestBrowseExplicitZeroDiskAllowed(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[browse]
max_disk_bytes = "0"
`)
	if err != nil {
		t.Fatalf("explicit max_disk_bytes = 0 should be allowed, got %v", err)
	}
	if cfg.Browse.MaxDiskBytes.Bytes() != 0 {
		t.Errorf("max_disk_bytes = %d, want 0", cfg.Browse.MaxDiskBytes.Bytes())
	}
}
