package resticx

import (
	"context"
	"fmt"
	"os"
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
			if got := repoURL(tt.t); got != tt.want {
				t.Errorf("repoURL = %q, want %q", got, tt.want)
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
	if len(last.Paths) != 2 || last.Paths[0] != "/etc" {
		t.Errorf("unexpected paths: %v", last.Paths)
	}
}

func TestStatsParsesFixture(t *testing.T) {
	fr := &fakeRunner{stdout: readFixture(t, "restic-stats-raw.json")}
	c := &Client{Runner: fr}
	s, err := c.Stats(context.Background(), testTarget, Creds{ResticPassword: "pw"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.TotalSize != 442000000000 || s.TotalBlobCount != 31204 || s.SnapshotsCount != 240 {
		t.Errorf("unexpected stats: %+v", s)
	}
	// Verify the args restic was actually invoked with.
	want := []string{"stats", "--json", "--mode", "raw-data"}
	if strings.Join(fr.gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("stats args = %v, want %v", fr.gotArgs, want)
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

func TestBucketLookupOption(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	tgt := testTarget
	tgt.BucketLookup = "dns"
	if _, err := c.Snapshots(context.Background(), tgt, Creds{ResticPassword: "pw"}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	got := strings.Join(fr.gotArgs, " ")
	if !strings.HasPrefix(got, "-o s3.bucket-lookup=dns snapshots") {
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
