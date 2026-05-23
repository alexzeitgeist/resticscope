package config

import (
	"strings"
	"testing"
	"time"
)

const minimalTOML = `
[global]
secrets_command = "cat ./test-secrets.json"

[[credentials]]
name     = "cred-a"
endpoint = "https://fsn1.your-objectstorage.com"
region   = "fsn1"

[[repos]]
name               = "repo-a"
credential         = "cred-a"
bucket             = "bucket-a"
expected_frequency = "24h"
`

// load decodes, normalizes (with a fixed home), and validates in one step,
// mirroring what Load does without touching the real $HOME.
func load(t *testing.T, data string) (*Config, error) {
	t.Helper()
	cfg, err := Decode([]byte(data))
	if err != nil {
		return nil, err
	}
	cfg.Normalize("/home/tester")
	return cfg, cfg.Validate()
}

func TestParsesMinimalConfig(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "repo-a" {
		t.Fatalf("unexpected repos: %+v", cfg.Repos)
	}
	if cfg.Repos[0].ExpectedFrequency.Std() != 24*time.Hour {
		t.Errorf("expected_frequency = %v, want 24h", cfg.Repos[0].ExpectedFrequency.Std())
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g := cfg.Global
	if g.Parallelism != defaultParallelism {
		t.Errorf("parallelism = %d, want %d", g.Parallelism, defaultParallelism)
	}
	if g.ShellPasswordMode != "file" {
		t.Errorf("shell_password_mode = %q, want file", g.ShellPasswordMode)
	}
	if g.StaleGrace.Std() != defaultStaleGrace {
		t.Errorf("stale_grace = %v, want %v", g.StaleGrace.Std(), defaultStaleGrace)
	}
	if cfg.Credentials[0].BucketLookup != "auto" {
		t.Errorf("bucket_lookup default = %q, want auto", cfg.Credentials[0].BucketLookup)
	}
}

func TestRefreshOnOpenDefaultsTrue(t *testing.T) {
	cfg, err := load(t, minimalTOML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Global.RefreshOnOpen {
		t.Error("refresh_on_open should default to true when the key is omitted")
	}
}

func TestRefreshOnOpenHonorsExplicitFalse(t *testing.T) {
	data := strings.Replace(minimalTOML, "secrets_command", "refresh_on_open = false\nsecrets_command", 1)
	cfg, err := load(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Global.RefreshOnOpen {
		t.Error("explicit refresh_on_open = false must be honored, got true")
	}
}

func TestExpandsHomePaths(t *testing.T) {
	cfg, err := load(t, minimalTOML+`
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/home/tester/.cache/resticscope"; cfg.Global.CacheDir != want {
		t.Errorf("cache_dir = %q, want %q", cfg.Global.CacheDir, want)
	}
	if want := "/home/tester/.cache/resticscope/log.jsonl"; cfg.Global.LogFile != want {
		t.Errorf("log_file = %q, want %q", cfg.Global.LogFile, want)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantSub string
	}{
		{
			name: "duplicate repo names",
			toml: minimalTOML + `
[[repos]]
name               = "repo-a"
credential         = "cred-a"
bucket             = "bucket-b"
expected_frequency = "24h"
`,
			wantSub: "duplicate repo name",
		},
		{
			name: "duplicate credential names",
			toml: minimalTOML + `
[[credentials]]
name     = "cred-a"
endpoint = "https://hel1.your-objectstorage.com"
`,
			wantSub: "duplicate credential name",
		},
		{
			name: "dangling credential reference",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
[[repos]]
name = "repo-a"
credential = "missing"
bucket = "b"
expected_frequency = "24h"
`,
			wantSub: "does not match any [[credentials]] block",
		},
		{
			name: "missing bucket",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
[[repos]]
name = "repo-a"
credential = "cred-a"
expected_frequency = "24h"
`,
			wantSub: "bucket is required",
		},
		{
			name: "missing endpoint",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
[[repos]]
name = "repo-a"
credential = "cred-a"
bucket = "b"
expected_frequency = "24h"
`,
			wantSub: "endpoint is required",
		},
		{
			name: "bad bucket_lookup",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
bucket_lookup = "wrong"
[[repos]]
name = "repo-a"
credential = "cred-a"
bucket = "b"
expected_frequency = "24h"
`,
			wantSub: "bucket_lookup must be",
		},
		{
			name: "repo name with path-unsafe characters",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
[[repos]]
name = "foo/bar"
credential = "cred-a"
bucket = "b"
expected_frequency = "24h"
`,
			wantSub: "name may contain only",
		},
		{
			name: "zero expected_frequency",
			toml: `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
[[repos]]
name = "repo-a"
credential = "cred-a"
bucket = "b"
expected_frequency = "0s"
`,
			wantSub: "expected_frequency must be a positive duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.toml)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestRejectsBadShellPasswordMode(t *testing.T) {
	_, err := load(t, `
[global]
secrets_command = "x"
shell_password_mode = "shout"
[[credentials]]
name = "cred-a"
endpoint = "https://e"
[[repos]]
name = "repo-a"
credential = "cred-a"
bucket = "b"
expected_frequency = "24h"
`)
	if err == nil || !strings.Contains(err.Error(), "shell_password_mode") {
		t.Fatalf("expected shell_password_mode error, got %v", err)
	}
}

func TestRejectsUnknownKeys(t *testing.T) {
	_, err := Decode([]byte(`
[global]
secrets_command = "x"
bogus_key = true
`))
	if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
		t.Fatalf("expected unknown-keys error, got %v", err)
	}
}

func TestRejectsBadDuration(t *testing.T) {
	_, err := Decode([]byte(`
[global]
secrets_command = "x"
stale_after = "soon"
`))
	if err == nil {
		t.Fatal("expected error for unparseable duration")
	}
}
