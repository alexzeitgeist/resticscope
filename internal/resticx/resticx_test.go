package resticx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner records what it was asked to run and returns canned output.
type fakeRunner struct {
	stdout, stderr []byte
	err            error
	block          bool // if set, block until ctx is done (to exercise timeout)

	gotEnv      []string
	gotPassword string
	gotArgs     []string
}

func (f *fakeRunner) Run(ctx context.Context, env []string, password string, args ...string) ([]byte, []byte, error) {
	f.gotEnv = env
	f.gotPassword = password
	f.gotArgs = args
	if f.block {
		<-ctx.Done()
		return nil, f.stderr, ctx.Err()
	}
	return f.stdout, f.stderr, f.err
}

// fakeExitError mimics *exec.ExitError for classification tests.
type fakeExitError int

func (e fakeExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExitError) ExitCode() int { return int(e) }

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

var testTarget = Target{
	Name: "homeserver-system",
	Repo: "s3:https://fsn1.your-objectstorage.com/homeserver-backups",
	Env:  map[string]string{"AWS_DEFAULT_REGION": "fsn1"},
}

func TestSnapshotsParsesFixture(t *testing.T) {
	fr := &fakeRunner{stdout: readFixture(t, "restic-0.18-snapshots.json")}
	c := &Client{Runner: fr}
	snaps, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(snaps))
	}
	last := snaps[2]
	if last.Hostname != "homeserver" || last.ShortID != "c3d4e5f6" {
		t.Errorf("unexpected last snapshot: %+v", last)
	}
	// Restic 0.17+ may omit a per-snapshot summary; nil must remain distinct from
	// a zero-byte snapshot.
	if last.Summary == nil {
		t.Fatal("expected the summarized snapshot to carry a Summary")
	}
	if last.Summary.TotalBytesProcessed != 4404019200 {
		t.Errorf("TotalBytesProcessed = %d, want 4404019200", last.Summary.TotalBytesProcessed)
	}
	if last.Summary.DataAdded == nil || *last.Summary.DataAdded != 5242880 {
		t.Errorf("DataAdded = %v, want 5242880", last.Summary.DataAdded)
	}
	if last.Summary.DataAddedPacked != nil {
		t.Errorf("DataAddedPacked = %d, want nil when omitted", *last.Summary.DataAddedPacked)
	}
	if last.Summary.FilesNew == nil || *last.Summary.FilesNew != 12 ||
		last.Summary.FilesChanged == nil || *last.Summary.FilesChanged != 34 ||
		last.Summary.TotalFilesProcessed == nil || *last.Summary.TotalFilesProcessed != 4096 {
		t.Errorf("file counts = new %v/changed %v/total %v, want 12/34/4096",
			last.Summary.FilesNew, last.Summary.FilesChanged, last.Summary.TotalFilesProcessed)
	}
	if last.Summary.BackupStart.IsZero() || last.Summary.BackupEnd.IsZero() {
		t.Errorf("expected non-zero backup_start/backup_end, got %v / %v", last.Summary.BackupStart, last.Summary.BackupEnd)
	}
	if snaps[0].Summary != nil {
		t.Errorf("expected nil Summary for the un-summarized snapshot, got %+v", snaps[0].Summary)
	}
	// Parse all snapshot-info fields from the same captured record.
	if last.Parent == "" || last.Tree == "" {
		t.Errorf("parent/tree not parsed: parent=%q tree=%q", last.Parent, last.Tree)
	}
	if len(last.Paths) != 2 || last.Paths[0] != "/etc" || last.Paths[1] != "/var/lib" {
		t.Errorf("Paths = %v, want [/etc /var/lib]", last.Paths)
	}
	if len(last.Excludes) != 2 || last.Excludes[0] != "*.tmp" {
		t.Errorf("Excludes = %v, want [*.tmp /var/cache]", last.Excludes)
	}
	// uid/gid of 0 must round-trip as present-and-zero (root), not absent.
	if last.UID == nil || *last.UID != 0 {
		t.Errorf("UID = %v, want 0 (root preserved)", last.UID)
	}
	if last.GID == nil || *last.GID != 0 {
		t.Errorf("GID = %v, want 0", last.GID)
	}
	if last.Summary.FilesUnmodified == nil || *last.Summary.FilesUnmodified != 4050 {
		t.Errorf("FilesUnmodified = %v, want 4050", last.Summary.FilesUnmodified)
	}
	if last.Summary.DirsNew == nil || *last.Summary.DirsNew != 1 ||
		last.Summary.DirsChanged == nil || *last.Summary.DirsChanged != 2 ||
		last.Summary.DirsUnmodified == nil || *last.Summary.DirsUnmodified != 7 {
		t.Errorf("dir counts = new %v/changed %v/unmodified %v, want 1/2/7",
			last.Summary.DirsNew, last.Summary.DirsChanged, last.Summary.DirsUnmodified)
	}
	if last.Summary.DataBlobs == nil || *last.Summary.DataBlobs != 11 {
		t.Errorf("DataBlobs = %v, want 11", last.Summary.DataBlobs)
	}
	if last.Summary.TreeBlobs == nil || *last.Summary.TreeBlobs != 3 {
		t.Errorf("TreeBlobs = %v, want 3", last.Summary.TreeBlobs)
	}
}

func TestEnvAndPasswordHandling(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr, CacheDir: "/cache"}
	creds := Creds{
		Env:            map[string]string{"AWS_ACCESS_KEY_ID": "AK-XYZ", "AWS_SECRET_ACCESS_KEY": "SK-XYZ"},
		ResticPassword: "super-secret-pw",
	}
	if _, err := c.Snapshots(t.Context(), testTarget, creds); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}

	env := strings.Join(fr.gotEnv, "\n")
	if !strings.Contains(env, "RESTIC_PASSWORD_FILE=/dev/fd/3") {
		t.Error("env should reference the password file on fd 3")
	}
	if !strings.Contains(env, "RESTIC_REPOSITORY=s3:https://fsn1.your-objectstorage.com/homeserver-backups") {
		t.Errorf("env missing repo URL: %q", env)
	}
	if !strings.Contains(env, "AWS_ACCESS_KEY_ID=AK-XYZ") {
		t.Error("env should carry the credential's backend env vars")
	}
	if !strings.Contains(env, "AWS_DEFAULT_REGION=fsn1") {
		t.Error("env should carry the target's backend env vars")
	}
	// The password must travel out-of-band, never in env or args.
	if strings.Contains(env, "super-secret-pw") {
		t.Error("password leaked into the environment")
	}
	if strings.Contains(strings.Join(fr.gotArgs, " "), "super-secret-pw") {
		t.Error("password leaked into the argument list")
	}
	if fr.gotPassword != "super-secret-pw" {
		t.Errorf("password not passed out-of-band, got %q", fr.gotPassword)
	}
}

func TestSnapshotsUsesNoLock(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	if _, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if got := strings.Join(fr.gotArgs, " "); got != "--no-lock snapshots --json" {
		t.Errorf("args = %q, want --no-lock snapshots --json", got)
	}
}

// Credential-free repositories must not inherit unrelated backend variables.
func TestEnvOmitsAbsentBackendVars(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	tgt := Target{Name: "local-repo", Repo: "/srv/restic-repo"}
	if _, err := c.Snapshots(t.Context(), tgt, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if strings.Contains(strings.Join(fr.gotEnv, "\n"), "AWS_") {
		t.Error("a target without backend env must not export AWS_* vars")
	}
}

// BackendEnviron sorts entries, gives credentials precedence, and rejects unsafe
// names again at the privileged process boundary.
func TestBackendEnvironMergesAndFilters(t *testing.T) {
	tgt := Target{
		Name: "r",
		Repo: "s3:https://host/b",
		Env: map[string]string{
			"AWS_DEFAULT_REGION": "fsn1",
			"SHARED":             "from-config",
		},
	}
	creds := Creds{Env: map[string]string{
		"SHARED":            "from-secret",
		"AWS_ACCESS_KEY_ID": "AK",
		"RESTIC_REPOSITORY": "s3:evil",  // reserved: resticscope owns it
		"LD_PRELOAD":        "/evil.so", // reserved prefix: linker injection
		"BAD NAME":          "x",        // not an env identifier
	}}
	got := BackendEnviron(tgt, creds)
	want := []string{
		"AWS_ACCESS_KEY_ID=AK",
		"AWS_DEFAULT_REGION=fsn1",
		"SHARED=from-secret",
	}
	if len(got) != len(want) {
		t.Fatalf("BackendEnviron = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BackendEnviron[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCatConfigReachable(t *testing.T) {
	fr := &fakeRunner{stdout: []byte(`{"version":2}`)}
	c := &Client{Runner: fr}
	if err := c.CatConfig(t.Context(), testTarget, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("CatConfig: %v", err)
	}
	if got := strings.Join(fr.gotArgs, " "); got != "--no-lock cat config" {
		t.Errorf("args = %q, want %q", got, "--no-lock cat config")
	}
	if fr.gotPassword != "pw" {
		t.Errorf("password not passed out-of-band, got %q", fr.gotPassword)
	}
}

func TestCatConfigClassifiesFailure(t *testing.T) {
	tests := []struct {
		name string
		exit fakeExitError
		want ErrorKind
	}{
		{"missing repo", fakeExitError(10), KindRepoNotFound},
		{"wrong password", fakeExitError(12), KindWrongPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{err: tt.exit}
			c := &Client{Runner: fr}
			err := c.CatConfig(t.Context(), testTarget, Creds{ResticPassword: "pw"})
			var re *Error
			if !asResticError(err, &re) {
				t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
			}
			if re.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", re.Kind, tt.want)
			}
		})
	}
}

func TestBackendOptionsPrepended(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	tgt := testTarget
	tgt.Options = map[string]string{"s3.bucket-lookup": "dns", "s3.connections": "8"}
	if _, err := c.Snapshots(t.Context(), tgt, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	got := strings.Join(fr.gotArgs, " ")
	if !strings.HasPrefix(got, "-o s3.bucket-lookup=dns -o s3.connections=8 --no-lock snapshots") {
		t.Errorf("expected backend options prepended in sorted order, got %q", got)
	}
}

func TestClassifyExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExitError(10), "", KindRepoNotFound},
		{"locked", fakeExitError(11), string(readFixture(t, "restic-error-locked.stderr")), KindLocked},
		{"wrong password", fakeExitError(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"unknown exit", fakeExitError(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{err: tt.err, stderr: []byte(tt.stderr)}
			c := &Client{Runner: fr}
			_, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"})
			var re *Error
			if !asResticError(err, &re) {
				t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
			}
			if re.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", re.Kind, tt.want)
			}
		})
	}
}

func TestClassifyTimeout(t *testing.T) {
	fr := &fakeRunner{block: true}
	c := &Client{Runner: fr, Timeout: time.Millisecond}
	_, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"})
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestExecRunnerTimeoutStopsRetryingRestic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	script := "#!/bin/sh\n" +
		"printf 'Save(<lock/abc123>) returned error, retrying after 1s: client.PutObject: The operation could not be performed\\n' >&2\n" +
		"while :; do :; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake restic: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Client{Runner: ExecRunner{}, Timeout: 100 * time.Millisecond}
	_, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"})
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("expected production runner timeout, got %T: %v", err, err)
	}
}

func TestClassifyCanceled(t *testing.T) {
	// Cancellation stays typed while unwrapping to context.Canceled.
	fr := &fakeRunner{block: true}
	c := &Client{Runner: fr}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Snapshots(ctx, testTarget, Creds{ResticPassword: "pw"})
	var re *Error
	if !asResticError(err, &re) {
		t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
	}
	if re.Kind != KindCanceled {
		t.Errorf("Kind = %v, want KindCanceled", re.Kind)
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is(err, context.Canceled) = false; want true (must unwrap to the sentinel)")
	}
}

func TestStderrRedactedInError(t *testing.T) {
	fr := &fakeRunner{err: fakeExitError(1), stderr: []byte("failed using key AK-LEAKED-123")}
	c := &Client{
		Runner: fr,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAKED-123", "[REDACTED]") },
	}
	_, err := c.Snapshots(t.Context(), testTarget, Creds{ResticPassword: "pw"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "AK-LEAKED-123") {
		t.Errorf("error leaked a secret in stderr: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("expected redacted stderr in error, got %q", err.Error())
	}
}

func TestBufferedMethodsRequireRunner(t *testing.T) {
	c := &Client{}
	creds := Creds{ResticPassword: "pw"}

	if _, err := c.Version(t.Context()); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Version error = %v, want ErrNoRunner", err)
	}
	if _, err := c.Snapshots(t.Context(), testTarget, creds); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Snapshots error = %v, want ErrNoRunner", err)
	}
	if err := c.CatConfig(t.Context(), testTarget, creds); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("CatConfig error = %v, want ErrNoRunner", err)
	}
	if _, err := c.FindMatches(t.Context(), testTarget, creds, "homeserver", "/etc/hostname"); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("FindMatches error = %v, want ErrNoRunner", err)
	}
}

// Cache layout helpers must match RESTIC_CACHE_DIR and cache-prune mapping.
func TestCacheLayoutHelpers(t *testing.T) {
	if got := CacheRoot(""); got != "" {
		t.Errorf("CacheRoot(\"\") = %q, want empty", got)
	}
	root := CacheRoot("/cache")
	if want := "/cache/restic-cache"; root != want {
		t.Errorf("CacheRoot = %q, want %q", root, want)
	}
	if got := RepoCacheName("home/server:1"); got != "home_server_1" {
		t.Errorf("RepoCacheName = %q, want home_server_1", got)
	}

	c := &Client{CacheDir: "/cache"}
	want := filepath.Join(root, RepoCacheName(testTarget.Name))
	if got := c.repoCacheDir(testTarget); got != want {
		t.Errorf("repoCacheDir = %q, want %q", got, want)
	}
	// Shell and refresh paths must remain identical.
	if got := RepoCacheDir("/cache", testTarget.Name); got != want {
		t.Errorf("RepoCacheDir = %q, want %q", got, want)
	}
	if got := RepoCacheDir("", testTarget.Name); got != "" {
		t.Errorf("RepoCacheDir(\"\", ...) = %q, want empty", got)
	}
}

func asResticError(err error, target **Error) bool {
	for err != nil {
		e := &Error{}
		if errors.As(err, &e) {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
