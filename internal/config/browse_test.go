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
	if b.MaxEntries != defaultBrowseMaxEntries {
		t.Errorf("max_entries = %d, want %d", b.MaxEntries, defaultBrowseMaxEntries)
	}
	if b.MaxJSONBytes.Bytes() != defaultBrowseMaxJSONBytes {
		t.Errorf("max_json_bytes = %d, want %d", b.MaxJSONBytes.Bytes(), defaultBrowseMaxJSONBytes)
	}
	if b.Timeout.Std() != defaultBrowseTimeout {
		t.Errorf("timeout = %v, want %v", b.Timeout.Std(), defaultBrowseTimeout)
	}
	if b.MaxSessionEntries != defaultBrowseMaxSessionEntries {
		t.Errorf("max_session_entries = %d, want %d", b.MaxSessionEntries, defaultBrowseMaxSessionEntries)
	}
	if b.MaxSessionJSONBytes.Bytes() != defaultBrowseMaxSessionJSONBytes {
		t.Errorf("max_session_json_bytes = %d, want %d", b.MaxSessionJSONBytes.Bytes(), defaultBrowseMaxSessionJSONBytes)
	}
}

func TestBrowseByteSizeParses(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[browse]
max_json_bytes = "64KiB"
max_session_json_bytes = "2MiB"
max_session_entries = 5000000
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Browse.MaxJSONBytes.Bytes(); got != 64<<10 {
		t.Errorf("max_json_bytes = %d, want %d", got, 64<<10)
	}
	if got := cfg.Browse.MaxSessionJSONBytes.Bytes(); got != 2<<20 {
		t.Errorf("max_session_json_bytes = %d, want %d", got, 2<<20)
	}
}

func TestBrowseByteSizeGiB(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
[browse]
max_json_bytes = "1GiB"
max_session_json_bytes = "1GiB"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Browse.MaxJSONBytes.Bytes(); got != 1<<30 {
		t.Errorf("max_json_bytes = %d, want %d", got, 1<<30)
	}
}

func TestBrowseValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		browse  string
		wantSub string
	}{
		{
			name:    "negative max_entries",
			browse:  "max_entries = -1",
			wantSub: "browse.max_entries must be positive",
		},
		{
			name:    "negative timeout survives normalize then fails validate",
			browse:  `timeout = "-5s"`,
			wantSub: "browse.timeout must be a positive duration",
		},
		{
			name:    "session entries below initial",
			browse:  "max_entries = 500000\nmax_session_entries = 1000",
			wantSub: "max_session_entries (1000) must be >= browse.max_entries (500000)",
		},
		{
			name:    "session bytes below initial",
			browse:  `max_json_bytes = "256MiB"` + "\n" + `max_session_json_bytes = "1MiB"`,
			wantSub: "max_session_json_bytes",
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
		_, err := Decode([]byte(minimalTOML + "\n[browse]\nmax_json_bytes = " + bad + "\n"))
		if err == nil {
			t.Errorf("expected decode error for max_json_bytes = %s, got nil", bad)
		}
	}
}

func TestBrowseRejectsUnknownKey(t *testing.T) {
	_, err := load(t, minimalTOML+`
[browse]
max_entries = 1000
bogus = 5
`)
	if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
		t.Fatalf("expected unknown-keys error, got %v", err)
	}
}

// An explicit zero is no longer treated like an omitted value. Browse caps are
// seeded with their defaults before decode, so a configured `0` overwrites the
// default and survives into validation, where it is rejected — letting a typo'd
// or deliberately-zeroed cap fail loudly instead of silently defaulting.
func TestBrowseExplicitZeroRejected(t *testing.T) {
	_, err := load(t, minimalTOML+`
[browse]
max_entries = 0
`)
	if err == nil {
		t.Fatal("expected validation error for explicit max_entries = 0, got nil")
	}
	if !strings.Contains(err.Error(), "browse.max_entries must be positive") {
		t.Errorf("error = %q, want substring %q", err.Error(), "browse.max_entries must be positive")
	}
}
