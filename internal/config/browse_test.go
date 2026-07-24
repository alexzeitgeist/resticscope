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

// Invalid and negative byte sizes fail during decoding, before defaults apply.
func TestBrowseRejectsBadByteSizeAtDecode(t *testing.T) {
	for _, bad := range []string{`"nonsense"`, `"-4MiB"`} {
		_, err := Decode([]byte(minimalTOML + "\n[browse]\nmax_disk_bytes = " + bad + "\n"))
		if err == nil {
			t.Errorf("expected decode error for max_disk_bytes = %s, got nil", bad)
		}
	}
}

// parseByteSize must reject values that overflow int64 rather than wrap; NaN and
// infinity are also out of range.
func TestParseByteSizeRange(t *testing.T) {
	for _, bad := range []string{"9000000000G", "8589934592G", "nan", "inf"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%q) = nil error, want out-of-range rejection", bad)
		}
	}
	const wantMax = int64(8589934591) << 30 // largest value below 2^63
	if got, err := parseByteSize("8589934591G"); err != nil || got != wantMax {
		t.Errorf("parseByteSize(%q) = %d, %v; want %d, nil", "8589934591G", got, err, wantMax)
	}
	if got, err := parseByteSize("2GiB"); err != nil || got != 2<<30 {
		t.Errorf("parseByteSize(%q) = %d, %v; want %d, nil", "2GiB", got, err, 2<<30)
	}
	if _, err := parseByteSize("-4MiB"); err == nil {
		t.Errorf("parseByteSize(%q) = nil error, want negative rejection", "-4MiB")
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

// A configured zero timeout overrides the seeded default and must fail validation.
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
