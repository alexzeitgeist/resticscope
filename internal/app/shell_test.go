package app

import (
	"os"
	"strings"
	"testing"

	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
)

var shellTarget = resticx.Target{
	Name:     "homeserver-system",
	Endpoint: "https://fsn1.your-objectstorage.com",
	Region:   "fsn1",
	Bucket:   "homeserver-backups",
}

var shellCreds = resticx.Creds{AccessKey: "AK-XYZ", SecretKey: "SK-XYZ", ResticPassword: "super-secret-pw"}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func TestResolveShell(t *testing.T) {
	tests := []struct {
		configured, env, want string
	}{
		{"/usr/bin/fish", "/bin/bash", "/usr/bin/fish"}, // config wins
		{"", "/bin/zsh", "/bin/zsh"},                    // $SHELL next
		{"", "", "/bin/sh"},                             // last resort
	}
	for _, tt := range tests {
		if got := resolveShell(tt.configured, tt.env); got != tt.want {
			t.Errorf("resolveShell(%q, %q) = %q, want %q", tt.configured, tt.env, got, tt.want)
		}
	}
}

func TestBuildShellEnvFileMode(t *testing.T) {
	env := buildShellEnv(nil, shellTarget, shellCreds, nil, "file", "/tmp/pw-123")

	if v, _ := envValue(env, "RESTIC_PASSWORD_FILE"); v != "/tmp/pw-123" {
		t.Errorf("RESTIC_PASSWORD_FILE = %q, want /tmp/pw-123", v)
	}
	if _, ok := envValue(env, "RESTIC_PASSWORD"); ok {
		t.Error("file mode must not export RESTIC_PASSWORD")
	}
	if v, _ := envValue(env, "RESTIC_REPOSITORY"); v != "s3:https://fsn1.your-objectstorage.com/homeserver-backups" {
		t.Errorf("RESTIC_REPOSITORY = %q", v)
	}
	if v, _ := envValue(env, "AWS_ACCESS_KEY_ID"); v != "AK-XYZ" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q", v)
	}
	if v, _ := envValue(env, "RESTICSCOPE_REPO"); v != "homeserver-system" {
		t.Errorf("RESTICSCOPE_REPO = %q", v)
	}
	if _, ok := envValue(env, "RESTICSCOPE_SNAPSHOT_ID"); ok {
		t.Error("no snapshot selected, RESTICSCOPE_SNAPSHOT_ID must be absent")
	}
	// The password must never reach the environment in file mode.
	if strings.Contains(strings.Join(env, "\n"), "super-secret-pw") {
		t.Error("password leaked into the shell environment in file mode")
	}
}

func TestBuildShellEnvEnvMode(t *testing.T) {
	env := buildShellEnv(nil, shellTarget, shellCreds, nil, "env", "")

	if v, _ := envValue(env, "RESTIC_PASSWORD"); v != "super-secret-pw" {
		t.Errorf("env mode should export RESTIC_PASSWORD, got %q", v)
	}
	if _, ok := envValue(env, "RESTIC_PASSWORD_FILE"); ok {
		t.Error("env mode must not set RESTIC_PASSWORD_FILE")
	}
}

func TestBuildShellEnvSnapshotContext(t *testing.T) {
	snap := &model.Snapshot{ID: "a1b2c3d4e5", ShortID: "a1b2c3d4"}
	env := buildShellEnv(nil, shellTarget, shellCreds, snap, "file", "/tmp/pw")
	if v, _ := envValue(env, "RESTICSCOPE_SNAPSHOT_ID"); v != "a1b2c3d4e5" {
		t.Errorf("RESTICSCOPE_SNAPSHOT_ID = %q, want the full id", v)
	}
}

func TestBuildShellEnvStripsInheritedOwnedVars(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"TERM=xterm",
		"RESTIC_PASSWORD=stale-leftover", // a stale value must not survive
		"AWS_ACCESS_KEY_ID=old-key",
		"RESTIC_REPOSITORY=s3:old",
	}
	env := buildShellEnv(base, shellTarget, shellCreds, nil, "file", "/tmp/pw")

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "stale-leftover") {
		t.Error("inherited RESTIC_PASSWORD must be stripped in file mode")
	}
	if strings.Contains(joined, "old-key") || strings.Contains(joined, "s3:old") {
		t.Error("inherited owned vars must be replaced, not duplicated")
	}
	// Exactly one RESTIC_REPOSITORY / AWS_ACCESS_KEY_ID, holding our value.
	if n := strings.Count(joined, "RESTIC_REPOSITORY="); n != 1 {
		t.Errorf("RESTIC_REPOSITORY appears %d times, want 1", n)
	}
	if v, _ := envValue(env, "AWS_ACCESS_KEY_ID"); v != "AK-XYZ" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want our value", v)
	}
	// The user's general environment is preserved.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/me", "TERM=xterm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env dropped inherited %q", want)
		}
	}
}

func TestShellBanner(t *testing.T) {
	url := "s3:https://fsn1.your-objectstorage.com/homeserver-backups"

	plain := shellBanner("homeserver-system", url, nil)
	if !strings.Contains(plain, "homeserver-system") || !strings.Contains(plain, url) {
		t.Errorf("banner missing repo/url:\n%s", plain)
	}
	if strings.Contains(plain, "RESTICSCOPE_SNAPSHOT_ID") {
		t.Error("banner mentions a snapshot id when none was selected")
	}

	withSnap := shellBanner("homeserver-system", url, &model.Snapshot{ID: "deadbeef"})
	if !strings.Contains(withSnap, "RESTICSCOPE_SNAPSHOT_ID=deadbeef") {
		t.Errorf("banner missing snapshot id:\n%s", withSnap)
	}
}

func TestInteractiveArgs(t *testing.T) {
	s := &ShellSession{Shell: "/bin/zsh", Banner: "hi there"}
	args := s.InteractiveArgs()
	if len(args) != 3 || args[0] != "/bin/sh" || args[1] != "-c" {
		t.Fatalf("args = %v, want [/bin/sh -c <script>]", args)
	}
	if !strings.Contains(args[2], "exec '/bin/zsh' -i") {
		t.Errorf("script does not exec the user's shell: %q", args[2])
	}
	if !strings.Contains(args[2], "'hi there'") {
		t.Errorf("script does not quote the banner: %q", args[2])
	}
}

func TestPosixQuoteEscapesQuotes(t *testing.T) {
	// A banner containing a single quote must not break out of the wrapper.
	got := posixQuote("it's a repo")
	if got != `'it'\''s a repo'` {
		t.Errorf("posixQuote = %q", got)
	}
}

// --- ShellSession integration: real temp file, fake secrets ---

type shellSecrets struct {
	mat secrets.Material
	err error
}

func (s shellSecrets) Resolve(_, _ string) (secrets.Material, error) { return s.mat, s.err }

func shellApp(mode string, sec Secrets) *App {
	cfg := &config.Config{
		Global:      config.Global{ShellPasswordMode: mode, Shell: "/bin/sh"},
		Credentials: []config.Credential{{Name: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto"}},
		Repos:       []config.Repo{{Name: "repo-a", Credential: "cred-a", Bucket: "b"}},
	}
	return &App{Cfg: cfg, Secrets: sec}
}

func TestShellSessionFileModeWritesAndCleansUp(t *testing.T) {
	a := shellApp("file", shellSecrets{mat: secrets.Material{AccessKey: "AK", SecretKey: "SK", ResticPassword: "pw-secret"}})

	sess, err := a.ShellSession("repo-a", nil)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}

	pwFile, ok := envValue(sess.Env, "RESTIC_PASSWORD_FILE")
	if !ok {
		t.Fatal("file mode did not set RESTIC_PASSWORD_FILE")
	}
	info, err := os.Stat(pwFile)
	if err != nil {
		t.Fatalf("stat password file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("password file mode = %o, want 600", perm)
	}
	contents, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	if string(contents) != "pw-secret" {
		t.Errorf("password file holds %q, want the restic password", contents)
	}

	if err := sess.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(pwFile); !os.IsNotExist(err) {
		t.Errorf("password file still present after Cleanup: %v", err)
	}
}

func TestShellSessionEnvModeNoFile(t *testing.T) {
	a := shellApp("env", shellSecrets{mat: secrets.Material{ResticPassword: "pw-secret"}})
	sess, err := a.ShellSession("repo-a", nil)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	if _, ok := envValue(sess.Env, "RESTIC_PASSWORD_FILE"); ok {
		t.Error("env mode must not create a password file")
	}
	if v, _ := envValue(sess.Env, "RESTIC_PASSWORD"); v != "pw-secret" {
		t.Errorf("env mode RESTIC_PASSWORD = %q", v)
	}
	if err := sess.Cleanup(); err != nil {
		t.Errorf("Cleanup in env mode should be a no-op, got %v", err)
	}
}

func TestShellSessionUnknownRepo(t *testing.T) {
	a := shellApp("file", shellSecrets{})
	if _, err := a.ShellSession("nope", nil); err == nil {
		t.Error("expected an error for an unknown repo")
	}
}

// A secrets failure must surface before any password file is created.
func TestShellSessionSecretsError(t *testing.T) {
	a := shellApp("file", shellSecrets{err: errResolve})
	if _, err := a.ShellSession("repo-a", nil); err == nil {
		t.Error("expected the secrets error to propagate")
	}
}

var errResolve = &resolveErr{}

type resolveErr struct{}

func (*resolveErr) Error() string { return "secrets: no repo" }
