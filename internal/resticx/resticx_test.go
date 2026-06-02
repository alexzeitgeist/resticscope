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

// fakeExit mimics *exec.ExitError for classification tests.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

var testTarget = Target{
	Name:         "homeserver-system",
	Endpoint:     "https://fsn1.your-objectstorage.com",
	Region:       "fsn1",
	BucketLookup: "auto",
	Bucket:       "homeserver-backups",
}

func TestRepoURLKeepsScheme(t *testing.T) {
	tests := []struct {
		name string
		t    Target
		want string
	}{
		{
			name: "bucket only",
			t:    Target{Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "b"},
			want: "s3:https://fsn1.your-objectstorage.com/b",
		},
		{
			name: "with path",
			t:    Target{Endpoint: "https://fsn1.your-objectstorage.com/", Bucket: "b", Path: "/sub/dir/"},
			want: "s3:https://fsn1.your-objectstorage.com/b/sub/dir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RepoURL(tt.t); got != tt.want {
				t.Errorf("RepoURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnapshotsParsesFixture(t *testing.T) {
	fr := &fakeRunner{stdout: readFixture(t, "restic-0.18-snapshots.json")}
	c := &Client{Runner: fr}
	snaps, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"})
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
	// restic 0.17+ embeds a per-snapshot summary; we parse its size for free. The
	// last entry carries one, the earlier entries do not — a nil Summary must stay
	// distinguishable from a zero-byte snapshot.
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
	// The richer summary fields restic records per backup must also parse.
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
	// The non-summary documented snapshot fields (parent, tree, paths, uid, gid,
	// excludes) and the additional documented summary counters back the
	// snapshot-info modal, so they must parse from the same fixture.
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
	creds := Creds{AccessKey: "AK-XYZ", SecretKey: "SK-XYZ", ResticPassword: "super-secret-pw"}
	if _, err := c.Snapshots(context.Background(), testTarget, creds); err != nil {
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
		t.Error("env should carry the S3 access key")
	}
	if !strings.Contains(env, "AWS_DEFAULT_REGION=fsn1") {
		t.Error("env should carry the repo region when one is set")
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
	if _, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if got := strings.Join(fr.gotArgs, " "); got != "--no-lock snapshots --json" {
		t.Errorf("args = %q, want --no-lock snapshots --json", got)
	}
}

// Region is optional. When a Target carries none, restic must get no
// AWS_DEFAULT_REGION at all rather than an empty one (Hetzner does not require
// it, and the endpoint host implies the region).
func TestEnvOmitsEmptyRegion(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	tgt := testTarget
	tgt.Region = ""
	if _, err := c.Snapshots(context.Background(), tgt, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if strings.Contains(strings.Join(fr.gotEnv, "\n"), "AWS_DEFAULT_REGION") {
		t.Error("an empty region must not export AWS_DEFAULT_REGION")
	}
}

func TestCatConfigReachable(t *testing.T) {
	fr := &fakeRunner{stdout: []byte(`{"version":2}`)}
	c := &Client{Runner: fr}
	if err := c.CatConfig(context.Background(), testTarget, Creds{ResticPassword: "pw"}); err != nil {
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
		exit fakeExit
		want ErrorKind
	}{
		{"missing repo", fakeExit(10), KindRepoNotFound},
		{"wrong password", fakeExit(12), KindWrongPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{err: tt.exit}
			c := &Client{Runner: fr}
			err := c.CatConfig(context.Background(), testTarget, Creds{ResticPassword: "pw"})
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

func TestBucketLookupOption(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	tgt := testTarget
	tgt.BucketLookup = "dns"
	if _, err := c.Snapshots(context.Background(), tgt, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	got := strings.Join(fr.gotArgs, " ")
	if !strings.HasPrefix(got, "-o s3.bucket-lookup=dns --no-lock snapshots") {
		t.Errorf("expected bucket-lookup option prepended, got %q", got)
	}
}

func TestClassifyExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExit(10), "", KindRepoNotFound},
		{"locked", fakeExit(11), string(readFixture(t, "restic-error-locked.stderr")), KindLocked},
		{"wrong password", fakeExit(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"unknown exit", fakeExit(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{err: tt.err, stderr: []byte(tt.stderr)}
			c := &Client{Runner: fr}
			_, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"})
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
	_, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"})
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
	_, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"})
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindTimeout {
		t.Fatalf("expected production runner timeout, got %T: %v", err, err)
	}
}

func TestClassifyCanceled(t *testing.T) {
	// A cancelled call must classify as a typed *resticx.Error (Kind KindCanceled)
	// that still unwraps to context.Canceled — not the bare sentinel, which would
	// break the "classify always returns *Error" contract every other caller relies
	// on (see TestClassifyExitCodes / TestClassifyTimeout).
	fr := &fakeRunner{block: true}
	c := &Client{Runner: fr}
	ctx, cancel := context.WithCancel(context.Background())
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
	fr := &fakeRunner{err: fakeExit(1), stderr: []byte("failed using key AK-LEAKED-123")}
	c := &Client{
		Runner: fr,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAKED-123", "[REDACTED]") },
	}
	_, err := c.Snapshots(context.Background(), testTarget, Creds{ResticPassword: "pw"})
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

// The exported cache-layout helpers must agree with the RESTIC_CACHE_DIR that
// real runs set, so `cache prune` maps configured repos to the right directory.
func TestCacheLayoutHelpers(t *testing.T) {
	if got := CacheRoot(""); got != "" {
		t.Errorf("CacheRoot(\"\") = %q, want empty", got)
	}
	root := CacheRoot("/cache")
	if want := "/cache/restic-cache"; root != want {
		t.Errorf("CacheRoot = %q, want %q", root, want)
	}
	// A name with path-unsafe characters is sanitized into one element.
	if got := RepoCacheName("home/server:1"); got != "home_server_1" {
		t.Errorf("RepoCacheName = %q, want home_server_1", got)
	}

	// The composed path must equal what buildEnv hands restic as RESTIC_CACHE_DIR.
	c := &Client{CacheDir: "/cache"}
	want := filepath.Join(root, RepoCacheName(testTarget.Name))
	if got := c.repoCacheDir(testTarget); got != want {
		t.Errorf("repoCacheDir = %q, want %q", got, want)
	}
	// The exported helper the shell uses must agree with the method, so refresh
	// and the shell warm byte-identical paths.
	if got := RepoCacheDir("/cache", testTarget.Name); got != want {
		t.Errorf("RepoCacheDir = %q, want %q", got, want)
	}
	if got := RepoCacheDir("", testTarget.Name); got != "" {
		t.Errorf("RepoCacheDir(\"\", ...) = %q, want empty", got)
	}
}

func asResticError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
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
