// Package resticx is the single boundary that knows how to execute restic.
//
// Nothing else in resticscope shells out to restic. The package wraps restic as
// a hostile external boundary (engineering rules, Rule 5): it builds the
// command environment, passes the repository password out-of-band, parses the
// --json output defensively, and classifies restic's exit codes into typed
// errors. It imports model only; config/secrets coordinates are passed in as
// plain structs so the layering stays clean.
package resticx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"resticscope/internal/model"
)

const defaultTimeout = 2 * time.Minute

// Target is the restic-facing coordinates of one repository, assembled by the
// caller from a config repo plus its credential.
type Target struct {
	Name         string // repo name; used for the per-repo restic cache subdir
	Endpoint     string // credential endpoint, scheme included (https://...)
	Region       string // credential region
	BucketLookup string // auto | dns | path
	Bucket       string // repo bucket
	Path         string // optional sub-prefix
}

// Creds are the resolved secret values for a Target. The restic password is
// passed to restic out-of-band (a pipe on fd 3), never on the command line.
type Creds struct {
	AccessKey      string
	SecretKey      string
	ResticPassword string
}

// Runner executes a single restic invocation. The production implementation
// (ExecRunner) passes password via fd 3; tests inject a fake. It returns stdout
// and stderr separately so callers can parse one and redact the other.
type Runner interface {
	Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error)
}

// Client runs restic commands against a Runner.
type Client struct {
	Runner   Runner
	CacheDir string              // resticscope cache root; restic's own cache goes under it
	Timeout  time.Duration       // per-invocation timeout; defaults to 2m
	Redact   func(string) string // optional; scrubs stderr before it enters an error
}

// Snapshots lists the repository's snapshots.
func (c *Client) Snapshots(ctx context.Context, t Target, creds Creds) ([]model.Snapshot, error) {
	out, err := c.run(ctx, t, creds, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	var snaps []model.Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, &Error{Kind: KindParse, Op: "snapshots", wrapped: err}
	}
	return snaps, nil
}

// Stats returns whole-repository raw-data stats (actual stored size and blob
// count). It is best-effort and slow on large repositories.
func (c *Client) Stats(ctx context.Context, t Target, creds Creds) (model.Stats, error) {
	out, err := c.run(ctx, t, creds, "stats", "--json", "--mode", "raw-data")
	if err != nil {
		return model.Stats{}, err
	}
	var s model.Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return model.Stats{}, &Error{Kind: KindParse, Op: "stats", wrapped: err}
	}
	return s, nil
}

// CatConfig reaches the repository by reading and decrypting its config file
// (`restic cat config`). It is the cheapest end-to-end probe: it exercises the
// S3 credentials, confirms the repository exists (exit 10 otherwise), and
// verifies the password decrypts it (exit 12 otherwise) — all without taking a
// lock. It returns a classified *Error on failure and nil when the repo is
// reachable; the decrypted config (stdout) is intentionally discarded, as it
// is not secret-free and `check` needs only the reachability verdict.
func (c *Client) CatConfig(ctx context.Context, t Target, creds Creds) error {
	_, err := c.run(ctx, t, creds, "cat", "config")
	return err
}

// Version returns restic's version string (e.g. "0.18.1"). It parses the plain
// `restic version` output, which is stable across the supported range.
func (c *Client) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	out, stderr, err := c.Runner.Run(ctx, minimalEnv(), "", "version")
	if err != nil {
		return "", c.classify(ctx, "version", err, stderr)
	}
	// "restic 0.18.1 compiled with go1.25.1 on linux/amd64"
	fields := strings.Fields(string(out))
	if len(fields) >= 2 && fields[0] == "restic" {
		return fields[1], nil
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Client) run(ctx context.Context, t Target, creds Creds, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	full := make([]string, 0, len(args)+2)
	if t.BucketLookup == "dns" || t.BucketLookup == "path" {
		full = append(full, "-o", "s3.bucket-lookup="+t.BucketLookup)
	}
	full = append(full, args...)

	env := c.buildEnv(t, creds)
	stdout, stderr, err := c.Runner.Run(ctx, env, creds.ResticPassword, full...)
	if err != nil {
		return nil, c.classify(ctx, args[0], err, stderr)
	}
	return stdout, nil
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// buildEnv constructs restic's environment from a known base. The repository
// password is NOT placed here; it is delivered via fd 3 and referenced by
// RESTIC_PASSWORD_FILE (engineering rules, Rule 9; plan §7).
func (c *Client) buildEnv(t Target, creds Creds) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"RESTIC_REPOSITORY=" + RepoURL(t),
		"RESTIC_CACHE_DIR=" + c.repoCacheDir(t),
		"AWS_ACCESS_KEY_ID=" + creds.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + creds.SecretKey,
		"AWS_DEFAULT_REGION=" + t.Region,
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
	}
}

func minimalEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
}

func (c *Client) repoCacheDir(t Target) string {
	root := CacheRoot(c.CacheDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, RepoCacheName(t.Name))
}

// CacheRoot returns the directory under which resticscope keeps restic's own
// per-repo caches (RESTIC_CACHE_DIR for each repo is a subdirectory named by
// RepoCacheName). It returns "" when cacheDir is empty. This is the layout
// `resticscope cache prune` scans, so the path lives here, with the runner that
// sets RESTIC_CACHE_DIR, as the single source of truth.
func CacheRoot(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "restic-cache")
}

// RepoCacheName returns the single path element under CacheRoot that holds a
// repo's restic cache. It matches the directory restic actually writes to under
// ExecRunner, so prune can map configured repos to their caches on disk.
func RepoCacheName(repoName string) string { return sanitize(repoName) }

// RepoURL builds restic's S3-compatible repository URL. The endpoint scheme is
// preserved — restic needs https:// to talk to non-AWS endpoints like Hetzner
// (plan §7). Form: s3:https://host[:port]/bucket[/path]. It is exported so the
// shell-out (internal/app) can set RESTIC_REPOSITORY identically.
func RepoURL(t Target) string {
	endpoint := strings.TrimRight(t.Endpoint, "/")
	u := "s3:" + endpoint + "/" + t.Bucket
	if p := strings.Trim(t.Path, "/"); p != "" {
		u += "/" + p
	}
	return u
}

// sanitize makes a repo name safe to use as a single path element.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
}
