package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
	"github.com/alexzeitgeist/resticscope/internal/secrets"
)

var shellTarget = resticx.Target{
	Name: "homeserver-system",
	Repo: "s3:https://fsn1.your-objectstorage.com/homeserver-backups",
	Env:  map[string]string{"AWS_DEFAULT_REGION": "fsn1"},
}

var shellCreds = resticx.Creds{
	Env:            map[string]string{"AWS_ACCESS_KEY_ID": "AK-XYZ", "AWS_SECRET_ACCESS_KEY": "SK-XYZ"},
	ResticPassword: "super-secret-pw",
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
	env := buildShellEnv(nil, shellEnvOpts{target: shellTarget, creds: shellCreds, mode: "file", pwFile: "/tmp/pw-123"})

	if v, _ := envLookup(env, "RESTIC_PASSWORD_FILE"); v != "/tmp/pw-123" {
		t.Errorf("RESTIC_PASSWORD_FILE = %q, want /tmp/pw-123", v)
	}
	if _, ok := envLookup(env, "RESTIC_PASSWORD"); ok {
		t.Error("file mode must not export RESTIC_PASSWORD")
	}
	if v, _ := envLookup(env, "RESTIC_REPOSITORY"); v != "s3:https://fsn1.your-objectstorage.com/homeserver-backups" {
		t.Errorf("RESTIC_REPOSITORY = %q", v)
	}
	if v, _ := envLookup(env, "AWS_ACCESS_KEY_ID"); v != "AK-XYZ" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q", v)
	}
	if v, _ := envLookup(env, "RESTICSCOPE_REPO"); v != "homeserver-system" {
		t.Errorf("RESTICSCOPE_REPO = %q", v)
	}
	if _, ok := envLookup(env, "RESTICSCOPE_SNAPSHOT_ID"); ok {
		t.Error("no snapshot selected, RESTICSCOPE_SNAPSHOT_ID must be absent")
	}
	// The password must never reach the environment in file mode.
	if strings.Contains(strings.Join(env, "\n"), "super-secret-pw") {
		t.Error("password leaked into the shell environment in file mode")
	}
}

func TestBuildShellEnvEnvMode(t *testing.T) {
	env := buildShellEnv(nil, shellEnvOpts{target: shellTarget, creds: shellCreds, mode: "env"})

	if v, _ := envLookup(env, "RESTIC_PASSWORD"); v != "super-secret-pw" {
		t.Errorf("env mode should export RESTIC_PASSWORD, got %q", v)
	}
	if _, ok := envLookup(env, "RESTIC_PASSWORD_FILE"); ok {
		t.Error("env mode must not set RESTIC_PASSWORD_FILE")
	}
}

func TestBuildShellEnvBackendVars(t *testing.T) {
	// The target's non-secret backend env is exported for the shell's tooling...
	env := buildShellEnv(nil, shellEnvOpts{target: shellTarget, creds: shellCreds, mode: "file", pwFile: "/tmp/pw"})
	if v, _ := envLookup(env, "AWS_DEFAULT_REGION"); v != "fsn1" {
		t.Errorf("AWS_DEFAULT_REGION = %q, want fsn1", v)
	}
	// ...and a target without it exports nothing extra.
	bare := resticx.Target{Name: "local-repo", Repo: "/srv/restic-repo"}
	env = buildShellEnv(nil, shellEnvOpts{target: bare, creds: resticx.Creds{ResticPassword: "pw"}, mode: "file", pwFile: "/tmp/pw"})
	if _, ok := envLookup(env, "AWS_DEFAULT_REGION"); ok {
		t.Error("a target without backend env must not export AWS_DEFAULT_REGION")
	}
}

// Whatever backend vars the session itself exports are also stripped from the
// inherited base, even when they are outside the static AWS family — the
// dynamic half of the owned-vars contract.
func TestBuildShellEnvStripsInheritedBackendVars(t *testing.T) {
	target := resticx.Target{Name: "nas-b2", Repo: "b2:bucket:repo"}
	creds := resticx.Creds{
		Env:            map[string]string{"B2_ACCOUNT_ID": "fresh-id", "B2_ACCOUNT_KEY": "fresh-key"},
		ResticPassword: "pw",
	}
	base := []string{"TERM=xterm", "B2_ACCOUNT_ID=stale-id"}
	env := buildShellEnv(base, shellEnvOpts{target: target, creds: creds, mode: "file", pwFile: "/tmp/pw"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "stale-id") {
		t.Error("inherited copy of a session-set backend var must be stripped")
	}
	if v, _ := envLookup(env, "B2_ACCOUNT_ID"); v != "fresh-id" {
		t.Errorf("B2_ACCOUNT_ID = %q, want our value", v)
	}
	if n := strings.Count(joined, "B2_ACCOUNT_ID="); n != 1 {
		t.Errorf("B2_ACCOUNT_ID appears %d times, want 1", n)
	}
}

func TestBuildShellEnvSetsCacheDir(t *testing.T) {
	// With a cache dir configured, the shell must export the exact per-repo path
	// the refresh runner warms, so a manual restic reuses that cache.
	env := buildShellEnv(nil, shellEnvOpts{target: shellTarget, cacheDir: "/home/me/.cache/resticscope", creds: shellCreds, mode: "file", pwFile: "/tmp/pw"})
	want := resticx.RepoCacheDir("/home/me/.cache/resticscope", shellTarget.Name)
	if v, _ := envLookup(env, "RESTIC_CACHE_DIR"); v != want {
		t.Errorf("RESTIC_CACHE_DIR = %q, want %q", v, want)
	}

	// With no cache dir configured, the var is omitted so restic falls back to
	// its own default rather than seeing an empty RESTIC_CACHE_DIR.
	env = buildShellEnv(nil, shellEnvOpts{target: shellTarget, creds: shellCreds, mode: "file", pwFile: "/tmp/pw"})
	if _, ok := envLookup(env, "RESTIC_CACHE_DIR"); ok {
		t.Error("unconfigured cache dir must not export RESTIC_CACHE_DIR")
	}
}

func TestBuildShellEnvSnapshotContext(t *testing.T) {
	snap := &model.Snapshot{ID: "a1b2c3d4e5", ShortID: "a1b2c3d4"}
	env := buildShellEnv(nil, shellEnvOpts{target: shellTarget, creds: shellCreds, snap: snap, mode: "file", pwFile: "/tmp/pw"})
	if v, _ := envLookup(env, "RESTICSCOPE_SNAPSHOT_ID"); v != "a1b2c3d4e5" {
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
		"AWS_SESSION_TOKEN=stale-session-token", // never paired with our static keys
		"RESTIC_REPOSITORY=s3:old",
		"RESTIC_CACHE_DIR=/stale/inherited",      // must be replaced by our per-repo path
		"B2_ACCOUNT_KEY=unrelated-b2-secret",     // another backend's credential must not ride along
		"AWS_PROFILE=other-tooling",              // deliberately kept: cannot affect restic, useful for the aws CLI
		"GOOGLE_APPLICATION_CREDENTIALS=/x.json", // exact-name credential family member
	}
	env := buildShellEnv(base, shellEnvOpts{target: shellTarget, cacheDir: "/test-cache", creds: shellCreds, mode: "file", pwFile: "/tmp/pw"})

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "stale-leftover") {
		t.Error("inherited RESTIC_PASSWORD must be stripped in file mode")
	}
	if strings.Contains(joined, "/stale/inherited") {
		t.Error("inherited RESTIC_CACHE_DIR must be stripped, not carried through")
	}
	if v, _ := envLookup(env, "RESTIC_CACHE_DIR"); v != resticx.RepoCacheDir("/test-cache", shellTarget.Name) {
		t.Errorf("RESTIC_CACHE_DIR = %q, want our per-repo path", v)
	}
	if n := strings.Count(joined, "RESTIC_CACHE_DIR="); n != 1 {
		t.Errorf("RESTIC_CACHE_DIR appears %d times, want 1", n)
	}
	if strings.Contains(joined, "old-key") || strings.Contains(joined, "s3:old") {
		t.Error("inherited owned vars must be replaced, not duplicated")
	}
	if _, ok := envLookup(env, "AWS_SESSION_TOKEN"); ok {
		t.Error("inherited AWS_SESSION_TOKEN must be stripped: pairing it with our static keys breaks S3 auth")
	}
	// Exactly one RESTIC_REPOSITORY / AWS_ACCESS_KEY_ID, holding our value.
	if n := strings.Count(joined, "RESTIC_REPOSITORY="); n != 1 {
		t.Errorf("RESTIC_REPOSITORY appears %d times, want 1", n)
	}
	if v, _ := envLookup(env, "AWS_ACCESS_KEY_ID"); v != "AK-XYZ" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want our value", v)
	}
	// Credentials of OTHER backends are stripped too, not just the families
	// this repo sets — the repo shell starts from the same credential-free
	// floor as the local shell.
	if strings.Contains(joined, "unrelated-b2-secret") {
		t.Error("inherited B2_ACCOUNT_KEY must be stripped from an s3 repo's shell")
	}
	if _, ok := envLookup(env, "GOOGLE_APPLICATION_CREDENTIALS"); ok {
		t.Error("inherited GOOGLE_APPLICATION_CREDENTIALS must be stripped")
	}
	// ...except the documented AWS profile pointers, which explicit env keys
	// always beat and the user may want for other tooling.
	if v, _ := envLookup(env, "AWS_PROFILE"); v != "other-tooling" {
		t.Errorf("AWS_PROFILE = %q, want the inherited value kept", v)
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
	if !strings.Contains(plain, "resticscope shell · homeserver-system") {
		t.Errorf("banner title should use the app-wide · separator:\n%s", plain)
	}
	if !strings.Contains(plain, url) {
		t.Errorf("banner missing repo url:\n%s", plain)
	}
	if strings.Contains(plain, "RESTICSCOPE_SNAPSHOT_ID") {
		t.Error("banner mentions a snapshot id when none was selected")
	}

	// A short id (tests, prefixes) is echoed verbatim.
	withSnap := shellBanner("homeserver-system", url, &model.Snapshot{ID: "deadbeef"})
	if !strings.Contains(withSnap, "RESTICSCOPE_SNAPSHOT_ID=deadbeef") {
		t.Errorf("banner missing snapshot id:\n%s", withSnap)
	}

	// A real 64-char id is shown as its 8-char prefix — the full value would
	// run to the terminal edge; the environment variable still carries it.
	longID := strings.Repeat("ab", 32)
	withLong := shellBanner("homeserver-system", url, &model.Snapshot{ID: longID})
	if strings.Contains(withLong, longID) {
		t.Errorf("banner echoes the full 64-char id:\n%s", withLong)
	}
	if !strings.Contains(withLong, "RESTICSCOPE_SNAPSHOT_ID=abababab…") {
		t.Errorf("banner missing the truncated id prefix:\n%s", withLong)
	}
}

func TestInteractiveArgs(t *testing.T) {
	s := &ShellSession{Shell: "/bin/zsh", Banner: "hi there"}
	args := s.InteractiveArgs()
	if len(args) != 3 || args[0] != "/bin/sh" || args[1] != "-c" {
		t.Fatalf("args = %v, want [/bin/sh -c <script>]", args)
	}
	if !strings.Contains(args[2], "exec '/bin/zsh' '-i'") {
		t.Errorf("script does not exec the user's shell: %q", args[2])
	}
	if !strings.Contains(args[2], "'hi there'") {
		t.Errorf("script does not quote the banner: %q", args[2])
	}
}

// An empty banner (the local "shell here" session) must emit only the exec
// wrapper, with no leading printf line that would print a blank line before the
// prompt.
func TestInteractiveArgsEmptyBanner(t *testing.T) {
	s := &ShellSession{Shell: "/bin/zsh"}
	args := s.InteractiveArgs()
	if len(args) != 3 || args[0] != "/bin/sh" || args[1] != "-c" {
		t.Fatalf("args = %v, want [/bin/sh -c <script>]", args)
	}
	if strings.Contains(args[2], "printf") {
		t.Errorf("empty banner must not emit a printf: %q", args[2])
	}
	if args[2] != "exec '/bin/zsh' '-i'" {
		t.Errorf("script = %q, want bare exec wrapper", args[2])
	}
}

// --- LocalShellSession ---

func localShellApp(shell string) *App {
	return &App{Cfg: &config.Config{Global: config.Global{Shell: shell}}}
}

func TestLocalShellSessionHappyPath(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	// A credential the parent process carries must not survive into the child.
	t.Setenv("RESTIC_PASSWORD", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk-leak")
	t.Setenv("RESTICSCOPE_REPO", "homeserver-system")

	dir := t.TempDir()
	a := localShellApp("") // no configured shell, so $SHELL wins
	sess, err := a.LocalShellSession(dir)
	if err != nil {
		t.Fatalf("LocalShellSession: %v", err)
	}
	if sess.Dir != dir {
		t.Errorf("Dir = %q, want %q", sess.Dir, dir)
	}
	if sess.Shell != "/bin/zsh" {
		t.Errorf("Shell = %q, want $SHELL", sess.Shell)
	}
	if sess.Banner != "" {
		t.Errorf("local shell must carry no banner, got %q", sess.Banner)
	}
	if sess.Cleanup == nil {
		t.Fatal("Cleanup must be a callable func, not nil")
	}
	// No password file exists here; Cleanup only removes prompt-tag scaffolding.
	if err := sess.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
	joined := strings.Join(sess.Env, "\n")
	for _, banned := range []string{"hunter2", "sk-leak", "homeserver-system"} {
		if strings.Contains(joined, banned) {
			t.Errorf("credential/context value %q leaked into the local shell env", banned)
		}
	}
	for _, key := range []string{"RESTIC_PASSWORD", "AWS_SECRET_ACCESS_KEY", "RESTICSCOPE_REPO"} {
		if _, ok := envLookup(sess.Env, key); ok {
			t.Errorf("local shell env still carries %s", key)
		}
	}
	// The user's general environment is preserved.
	if _, ok := envLookup(sess.Env, "PATH"); !ok {
		t.Error("local shell dropped PATH; the shell would be unusable")
	}
}

func TestLocalShellSessionFallbackShell(t *testing.T) {
	t.Setenv("SHELL", "")
	a := localShellApp("") // neither configured nor $SHELL
	sess, err := a.LocalShellSession(t.TempDir())
	if err != nil {
		t.Fatalf("LocalShellSession: %v", err)
	}
	if sess.Shell != "/bin/sh" {
		t.Errorf("fallback Shell = %q, want /bin/sh", sess.Shell)
	}
}

func TestLocalShellSessionUnenterableDirFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("canEnterDir probes only on linux/darwin")
	}
	if os.Getuid() == 0 {
		t.Skip("root can enter anything; the fallback never triggers")
	}
	// Models a privileged extract's target: the extracted node itself is
	// enterable only by its (root) owner while the parent scaffolding belongs
	// to the user. Chmod 0 on an own dir denies ourselves the same way.
	parent := t.TempDir()
	dir := filepath.Join(parent, "rootowned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let TempDir cleanup descend

	a := localShellApp("/bin/sh")
	sess, err := a.LocalShellSession(dir)
	if err != nil {
		t.Fatalf("LocalShellSession: %v", err)
	}
	if sess.Dir != parent {
		t.Errorf("Dir = %q, want the enterable parent %q", sess.Dir, parent)
	}
	// The relocation must explain itself; a silent parent drop reads as a bug.
	if !strings.Contains(sess.Banner, "sudo") {
		t.Errorf("Banner = %q, want a fallback explanation mentioning sudo", sess.Banner)
	}
}

func TestLocalShellSessionInvalidDir(t *testing.T) {
	a := localShellApp("/bin/sh")

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	tests := []struct {
		name, dir, wantReason string
	}{
		{"empty", "", "dir empty"},
		{"relative", "relative/dir", "dir not absolute"},
		{"missing", missing, "dir not accessible"},
		{"regular file", file, "dir not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.LocalShellSession(tt.dir)
			if err == nil {
				t.Fatalf("LocalShellSession(%q) returned no error", tt.dir)
			}
			if !errors.Is(err, ErrLocalShellInvalidDir) {
				t.Errorf("error %v is not ErrLocalShellInvalidDir", err)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error %q does not name the failed check %q", err, tt.wantReason)
			}
			// The dir value must never appear in the error (privacy contract §3).
			if tt.dir != "" && strings.Contains(err.Error(), tt.dir) {
				t.Errorf("error %q leaked the dir value", err)
			}
		})
	}
}

func TestStripCredEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"TERM=xterm",
		"RESTIC_PASSWORD=secret",
		"RESTIC_REPOSITORY=s3:x",
		"AWS_ACCESS_KEY_ID=ak",
		"AWS_SECRET_ACCESS_KEY=sk",
		"B2_ACCOUNT_ID=b2",
		"GOOGLE_APPLICATION_CREDENTIALS=/path/to/creds.json",
		"RESTICSCOPE_SNAPSHOT_ID=deadbeef",
		"MALFORMED_NO_EQUALS", // passed through untouched
	}
	got := stripCredEnv(base)
	joined := strings.Join(got, "\n")

	for _, gone := range []string{
		"RESTIC_PASSWORD", "RESTIC_REPOSITORY", "AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY", "B2_ACCOUNT_ID", "GOOGLE_APPLICATION_CREDENTIALS",
		"RESTICSCOPE_SNAPSHOT_ID",
	} {
		if strings.Contains(joined, gone+"=") {
			t.Errorf("stripCredEnv kept credential var %s", gone)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/me", "TERM=xterm", "MALFORMED_NO_EQUALS"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("stripCredEnv dropped non-credential entry %q", kept)
		}
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
		Global: config.Global{ShellPasswordMode: mode, Shell: "/bin/sh", CacheDir: "/test-cache"},
		Repos:  []config.Repo{{Name: "repo-a", Credential: "cred-a", Endpoint: "https://e", Region: "fsn1", BucketLookup: "auto", Bucket: "b"}},
	}
	return &App{Cfg: cfg, Secrets: sec}
}

func TestShellSessionFileModeWritesAndCleansUp(t *testing.T) {
	a := shellApp("file", shellSecrets{mat: secrets.Material{
		Env:            map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"},
		ResticPassword: "pw-secret",
	}})

	sess, err := a.ShellSession("repo-a", nil)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}

	pwFile, ok := envLookup(sess.Env, "RESTIC_PASSWORD_FILE")
	if !ok {
		t.Fatal("file mode did not set RESTIC_PASSWORD_FILE")
	}

	// The session must export the same per-repo cache dir the refresh runner
	// uses, wired through from Global.CacheDir.
	if v, _ := envLookup(sess.Env, "RESTIC_CACHE_DIR"); v != resticx.RepoCacheDir("/test-cache", "repo-a") {
		t.Errorf("RESTIC_CACHE_DIR = %q, want the per-repo cache path", v)
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
	if _, ok := envLookup(sess.Env, "RESTIC_PASSWORD_FILE"); ok {
		t.Error("env mode must not create a password file")
	}
	if v, _ := envLookup(sess.Env, "RESTIC_PASSWORD"); v != "pw-secret" {
		t.Errorf("env mode RESTIC_PASSWORD = %q", v)
	}
	if err := sess.Cleanup(); err != nil {
		t.Errorf("Cleanup in env mode should be a no-op, got %v", err)
	}
}

func TestShellSessionUnknownRepo(t *testing.T) {
	a := shellApp("file", shellSecrets{})
	if _, err := a.ShellSession("nope", nil); !errors.Is(err, ErrUnknownRepo) {
		t.Errorf("err = %v, want ErrUnknownRepo", err)
	}
}

// A secrets failure must surface before any password file is created.
func TestShellSessionSecretsError(t *testing.T) {
	a := shellApp("file", shellSecrets{err: errResolve})
	if _, err := a.ShellSession("repo-a", nil); err == nil {
		t.Error("expected the secrets error to propagate")
	}
}

var errResolve = &resolveError{}

type resolveError struct{}

func (*resolveError) Error() string { return "secrets: no repo" }
