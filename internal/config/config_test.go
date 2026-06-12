package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalTOML = `
[global]
secrets_command = "cat ./test-secrets.json"

[[credentials]]
name = "cred-a"

[[repos]]
name               = "repo-a"
credential         = "cred-a"
endpoint           = "https://fsn1.your-objectstorage.com"
region             = "fsn1"
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
	if cfg.Repos[0].BucketLookup != "auto" {
		t.Errorf("bucket_lookup default = %q, want auto", cfg.Repos[0].BucketLookup)
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
name = "cred-a"
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
[[repos]]
name = "repo-a"
credential = "missing"
endpoint = "https://e"
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
[[repos]]
name = "repo-a"
credential = "cred-a"
endpoint = "https://e"
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
[[repos]]
name = "repo-a"
credential = "cred-a"
endpoint = "https://e"
bucket = "b"
bucket_lookup = "wrong"
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
[[repos]]
name = "foo/bar"
credential = "cred-a"
endpoint = "https://e"
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
[[repos]]
name = "repo-a"
credential = "cred-a"
endpoint = "https://e"
bucket = "b"
expected_frequency = "0s"
`,
			wantSub: "expected_frequency must be a positive duration",
		},
		{
			name: "url mixed with s3 shorthand",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "sftp:u@h:/srv/repo"
bucket = "b"
expected_frequency = "24h"
`,
			wantSub: "mutually exclusive",
		},
		{
			name: "url with unknown scheme",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "stfp:u@h:/srv/repo"
expected_frequency = "24h"
`,
			wantSub: "is not a restic backend",
		},
		{
			name: "url with relative path",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "srv/repo"
expected_frequency = "24h"
`,
			wantSub: "url must be a restic repository",
		},
		{
			name: "reserved env name",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "/srv/repo"
env = { RESTIC_PASSWORD = "nope" }
expected_frequency = "24h"
`,
			wantSub: "reserved by resticscope",
		},
		{
			name: "invalid env name",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "/srv/repo"
env = { "BAD-NAME" = "v" }
expected_frequency = "24h"
`,
			wantSub: "not a valid environment variable name",
		},
		{
			name: "empty env value",
			toml: `
[global]
secrets_command = "x"
[[repos]]
name = "repo-a"
url = "/srv/repo"
env = { GOOGLE_PROJECT_ID = "" }
expected_frequency = "24h"
`,
			wantSub: "must not be empty",
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
[[repos]]
name = "repo-a"
credential = "cred-a"
endpoint = "https://e"
bucket = "b"
expected_frequency = "24h"
`)
	if err == nil || !strings.Contains(err.Error(), "shell_password_mode") {
		t.Fatalf("expected shell_password_mode error, got %v", err)
	}
}

// region is optional: the endpoint host usually implies it, so a repo that omits
// it must load and validate cleanly (and refresh exports no AWS_DEFAULT_REGION).
func TestOmittedRegionAccepted(t *testing.T) {
	cfg, err := load(t, `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
[[repos]]
name = "repo-a"
credential = "cred-a"
endpoint = "https://hel1.your-objectstorage.com"
bucket = "b"
expected_frequency = "24h"
`)
	if err != nil {
		t.Fatalf("omitted region should be accepted, got %v", err)
	}
	if cfg.Repos[0].Region != "" {
		t.Errorf("region = %q, want empty", cfg.Repos[0].Region)
	}
}

// A url repo is the storage-agnostic form: any restic backend, an optional
// credential, generic env/options. The s3 shorthand keeps working beside it.
func TestURLRepoForms(t *testing.T) {
	cfg, err := load(t, `
[global]
secrets_command = "x"

[[credentials]]
name = "b2-home"

[[repos]]
name               = "local-disk"
url                = "/srv/restic-repo"
expected_frequency = "24h"

[[repos]]
name               = "homedir-disk"
url                = "~/restic-repo"
expected_frequency = "24h"

[[repos]]
name               = "nas"
url                = "sftp:backup@nas:/srv/restic-repo"
options            = { "sftp.command" = "ssh -i /home/me/.ssh/nas backup@nas -s sftp" }
expected_frequency = "24h"

[[repos]]
name               = "cloud-b2"
url                = "b2:bucket-name:repo"
credential         = "b2-home"
expected_frequency = "24h"

[[repos]]
name               = "gcs"
url                = "gs:bucket:/"
credential         = "b2-home"
env                = { GOOGLE_PROJECT_ID = "proj-123" }
expected_frequency = "24h"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	local := cfg.Repos[0]
	if local.RepositoryURL() != "/srv/restic-repo" || local.Backend() != "local" {
		t.Errorf("local repo: url %q backend %q", local.RepositoryURL(), local.Backend())
	}
	if local.Credential != "" {
		t.Errorf("credential should be optional, got %q", local.Credential)
	}
	if local.BucketLookup != "" {
		t.Errorf("bucket_lookup must not be seeded for url repos, got %q", local.BucketLookup)
	}
	if len(local.BackendOptions()) != 0 || len(local.BackendEnv()) != 0 {
		t.Errorf("local repo should carry no options/env, got %v / %v", local.BackendOptions(), local.BackendEnv())
	}

	if got := cfg.Repos[1].RepositoryURL(); got != "/home/tester/restic-repo" {
		t.Errorf("~ in a bare-path url must expand, got %q", got)
	}

	nas := cfg.Repos[2]
	if nas.Backend() != "sftp" {
		t.Errorf("nas backend = %q, want sftp", nas.Backend())
	}
	if got := nas.BackendOptions()["sftp.command"]; !strings.Contains(got, "ssh -i") {
		t.Errorf("options not carried through, got %v", nas.BackendOptions())
	}

	if b2 := cfg.Repos[3]; b2.Backend() != "b2" {
		t.Errorf("b2 backend = %q, want b2", b2.Backend())
	}

	gcs := cfg.Repos[4]
	if gcs.Backend() != "gs" || gcs.BackendEnv()["GOOGLE_PROJECT_ID"] != "proj-123" {
		t.Errorf("gs repo: backend %q env %v", gcs.Backend(), gcs.BackendEnv())
	}
}

// The s3 shorthand lowers onto the same generic surface the url form uses:
// RepositoryURL assembles the s3 URL, region becomes AWS_DEFAULT_REGION, and a
// non-auto bucket_lookup becomes the s3.bucket-lookup option.
func TestS3ShorthandLowering(t *testing.T) {
	cfg, err := load(t, `
[global]
secrets_command = "x"
[[credentials]]
name = "cred-a"
[[repos]]
name               = "repo-a"
credential         = "cred-a"
endpoint           = "https://fsn1.your-objectstorage.com/"
region             = "fsn1"
bucket             = "bucket-a"
path               = "/sub/dir/"
bucket_lookup      = "dns"
expected_frequency = "24h"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := cfg.Repos[0]
	if want := "s3:https://fsn1.your-objectstorage.com/bucket-a/sub/dir"; r.RepositoryURL() != want {
		t.Errorf("RepositoryURL = %q, want %q", r.RepositoryURL(), want)
	}
	if r.Backend() != "s3" {
		t.Errorf("Backend = %q, want s3", r.Backend())
	}
	if got := r.BackendEnv(); len(got) != 1 || got["AWS_DEFAULT_REGION"] != "fsn1" {
		t.Errorf("BackendEnv = %v, want only AWS_DEFAULT_REGION=fsn1", got)
	}
	if got := r.BackendOptions(); len(got) != 1 || got["s3.bucket-lookup"] != "dns" {
		t.Errorf("BackendOptions = %v, want only s3.bucket-lookup=dns", got)
	}
}

// group_by is an optional [global] field listing the repo-label keys the list
// view can cycle through. A present value must decode into Global.GroupBy as a
// slice without tripping the unknown-keys gate, and omitted/explicit-empty
// forms must both normalize to the same non-nil empty slice so runtime code can
// rely on a single len-based check.
func TestParsesGroupBy(t *testing.T) {
	cfg, err := load(t, strings.Replace(minimalTOML, "secrets_command", `group_by = ["env", "region"]
secrets_command`, 1))
	if err != nil {
		t.Fatalf("group_by should decode cleanly, got %v", err)
	}
	if got, want := cfg.Global.GroupBy, []string{"env", "region"}; !slices.Equal(got, want) {
		t.Errorf("group_by = %v, want %v", got, want)
	}

	// Omitting the key leaves Global.GroupBy as a non-nil empty slice.
	cfg, err = load(t, minimalTOML)
	if err != nil {
		t.Fatalf("baseline decode failed: %v", err)
	}
	if cfg.Global.GroupBy == nil {
		t.Error("omitted group_by should normalize to a non-nil empty slice, got nil")
	}
	if len(cfg.Global.GroupBy) != 0 {
		t.Errorf("omitted group_by = %v, want empty slice", cfg.Global.GroupBy)
	}

	// Explicit empty list normalizes to the same non-nil empty slice.
	cfg, err = load(t, strings.Replace(minimalTOML, "secrets_command", `group_by = []
secrets_command`, 1))
	if err != nil {
		t.Fatalf("explicit empty group_by should decode cleanly, got %v", err)
	}
	if cfg.Global.GroupBy == nil {
		t.Error("explicit empty group_by should normalize to a non-nil empty slice, got nil")
	}
	if len(cfg.Global.GroupBy) != 0 {
		t.Errorf("explicit empty group_by = %v, want empty slice", cfg.Global.GroupBy)
	}
}

// The legacy single-string form must now fail decode so users see the schema
// break loudly instead of silently dropping their configured grouping.
func TestRejectsLegacyGroupByString(t *testing.T) {
	_, err := load(t, strings.Replace(minimalTOML, "secrets_command", `group_by = "category"
secrets_command`, 1))
	if err == nil {
		t.Fatal("expected legacy string group_by to fail decode")
	}
}

// Validation must surface a global.group_by error for empty, whitespace-padded,
// and duplicate entries so the user can fix the offending index without guessing.
func TestValidatesGroupByEntries(t *testing.T) {
	cases := []struct {
		name     string
		listTOML string
		wantSub  string
	}{
		{"empty", `["env", ""]`, "global.group_by[1]"},
		{"whitespace_padded", `[" env"]`, "global.group_by[0]"},
		{"duplicate", `["env", "env"]`, "global.group_by[1]"},
		{"whitespace_duplicate", `[" env", "env"]`, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := strings.Replace(minimalTOML, "secrets_command", `group_by = `+tc.listTOML+`
secrets_command`, 1)
			_, err := load(t, toml)
			if err == nil {
				t.Fatalf("expected validation error for group_by = %s", tc.listTOML)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantSub)
			}
			if !strings.Contains(err.Error(), "group_by") {
				t.Errorf("error %q should reference group_by", err.Error())
			}
		})
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

// The coverage feature was removed: a pre-cleanup config that still declares
// expected_hosts/paths/tags must fail decode with an unknown-keys error so the
// user deletes them, rather than silently ignoring stale expectations.
func TestRejectsRemovedCoverageKeys(t *testing.T) {
	for _, key := range []string{"expected_hosts", "expected_paths", "expected_tags"} {
		t.Run(key, func(t *testing.T) {
			toml := minimalTOML + key + ` = ["x"]` + "\n"
			_, err := load(t, toml)
			if err == nil || !strings.Contains(err.Error(), "unknown config keys") {
				t.Fatalf("expected unknown-keys error for %s, got %v", key, err)
			}
		})
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
