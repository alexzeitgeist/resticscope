// Package resticx is the single boundary that knows how to execute restic.
//
// Nothing else in resticscope shells out to restic. The package wraps restic as
// a hostile external boundary (engineering rules, Rule 5): it builds the
// command environment, passes the repository password out-of-band (i.e. never on argv), parses the
// --json output defensively, and classifies restic's exit codes into typed
// errors. It imports model only; config/secrets coordinates are passed in as
// plain structs so the layering stays clean.
package resticx

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"resticscope/internal/model"
)

const defaultTimeout = 2 * time.Minute

// Target is the restic-facing coordinates of one repository, assembled by the
// caller from a config repo. It is backend-agnostic: Repo is the repository
// string restic itself understands (any backend), Options are the repo's
// restic -o backend options, and Env is its non-secret backend environment.
// Secret env vars ride in Creds, never here.
type Target struct {
	Name    string            // repo name; used for the per-repo restic cache subdir
	Repo    string            // RESTIC_REPOSITORY, e.g. "s3:https://host/bucket" or "sftp:user@host:/srv/repo"
	Options map[string]string // restic -o key=value backend options
	Env     map[string]string // non-secret backend env vars (e.g. AWS_DEFAULT_REGION)
}

// Creds are the resolved secret values for a Target: the credential's backend
// env vars (AWS_*, B2_*, AZURE_*, ... — nil for backends that need none) plus
// the repository password, which is passed to restic out-of-band (a pipe on
// fd 3), never on the command line.
type Creds struct {
	Env            map[string]string
	ResticPassword string
}

// Runner executes a single restic invocation. The production implementation
// (ExecRunner) passes password via fd 3; tests inject a fake. It returns stdout
// and stderr separately so callers can parse one and redact the other.
type Runner interface {
	Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error)
}

// StreamRunner executes a restic invocation whose stdout is consumed as a
// stream rather than buffered whole. onStdout is called on the runner's calling
// goroutine with a reader over restic's stdout; the runner returns once onStdout
// returns and the process exits. It uses the same out-of-band password delivery
// as Runner. It is a separate seam from Runner so the buffered path is untouched
// and the streaming caps/cancellation are testable without a real restic.
type StreamRunner interface {
	RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error)
}

// PatternStreamRunner is the optional streaming capability for invocations
// that also deliver a pattern-file payload out-of-band on fd 4 (the argv
// references it as /dev/fd/4) — the multi-include restore shape. The
// production ExecRunner implements it; ExtractTree type-asserts for it only
// when a run actually carries a pattern file, so fakes and runners for the
// other streams can stay plain StreamRunners.
type PatternStreamRunner interface {
	StreamRunner
	RunStreamPatterns(ctx context.Context, env []string, password string, patterns []byte, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error)
}

// Client runs restic commands against a Runner.
type Client struct {
	Runner   Runner
	Stream   StreamRunner        // streaming path for browse; production wires ExecRunner{}
	CacheDir string              // resticscope cache root; restic's own cache goes under it
	Timeout  time.Duration       // per-invocation timeout; defaults to 2m
	Redact   func(string) string // optional; scrubs stderr before it enters an error
}

// Snapshots lists the repository's snapshots.
func (c *Client) Snapshots(ctx context.Context, t Target, creds Creds) ([]model.Snapshot, error) {
	// snapshots is read-only. Running it lockless keeps resticscope usable for
	// repositories that can still be read but reject lock writes, for example a
	// provider-side write hold.
	out, err := c.runOp(ctx, t, creds, "snapshots", "--no-lock", "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	var snaps []model.Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, &Error{Kind: KindParse, Op: "snapshots", wrapped: err}
	}
	return snaps, nil
}

// CatConfig reaches the repository by reading and decrypting its config file
// (`restic cat config`). It is the cheapest end-to-end probe: it exercises the
// backend credentials, confirms the repository exists (exit 10 otherwise), and
// verifies the password decrypts it (exit 12 otherwise) — all without taking a
// lock. It returns a classified *Error on failure and nil when the repo is
// reachable; the decrypted config (stdout) is intentionally discarded, as it
// is not secret-free and `check` needs only the reachability verdict.
func (c *Client) CatConfig(ctx context.Context, t Target, creds Creds) error {
	_, err := c.runOp(ctx, t, creds, "cat", "--no-lock", "cat", "config")
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

func (c *Client) runOp(ctx context.Context, t Target, creds Creds, op string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	full := prependBackendOpts(t, args...)

	env := c.buildEnv(t, creds)
	stdout, stderr, err := c.Runner.Run(ctx, env, creds.ResticPassword, full...)
	if err != nil {
		return nil, c.classify(ctx, op, err, stderr)
	}
	return stdout, nil
}

// prependBackendOpts returns args with the target's backend -o options
// prepended, in sorted key order so argv is deterministic. The single assembly
// point for every driver (runOp and the browse/diff/restore streams), so a new
// backend option means one config entry, not code.
func prependBackendOpts(t Target, args ...string) []string {
	full := make([]string, 0, len(args)+2*len(t.Options))
	for _, k := range sortedKeys(t.Options) {
		full = append(full, "-o", k+"="+t.Options[k])
	}
	return append(full, args...)
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
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"RESTIC_REPOSITORY=" + t.Repo,
		"RESTIC_CACHE_DIR=" + c.repoCacheDir(t),
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
	}
	return append(env, BackendEnviron(t, creds)...)
}

// BackendEnviron flattens the target's and credential's backend env maps into
// sorted KEY=value entries — Target.Env (non-secret, from config) merged with
// Creds.Env (secret), the secret value winning a key collision. It is exported
// so the repo shell-out (internal/app) exports the identical environment.
//
// Reserved and invalid names are dropped here, not just rejected at
// config/secrets validation: this assembly also runs inside the privileged
// extract helper on a payload that crossed a process boundary, and the helper
// runs restic as root, so the chokepoint enforces the model policy itself
// (RESTIC_*/PATH/HOME ownership, no LD_PRELOAD-style injection).
func BackendEnviron(t Target, creds Creds) []string {
	if len(t.Env) == 0 && len(creds.Env) == 0 {
		return nil
	}
	merged := make(map[string]string, len(t.Env)+len(creds.Env))
	for _, m := range []map[string]string{t.Env, creds.Env} {
		for k, v := range m {
			if !model.ValidBackendEnvName(k) || model.ReservedBackendEnvName(k) {
				continue
			}
			merged[k] = v
		}
	}
	env := make([]string, 0, len(merged))
	for _, k := range sortedKeys(merged) {
		env = append(env, k+"="+merged[k])
	}
	return env
}

// sortedKeys returns m's keys sorted, so env and argv assembly stay
// deterministic across runs (maps iterate in random order).
func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

func minimalEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
}

func (c *Client) repoCacheDir(t Target) string {
	return RepoCacheDir(c.CacheDir, t.Name)
}

// RepoCacheDir returns the RESTIC_CACHE_DIR for one repo — the per-repo
// subdirectory under CacheRoot that holds its restic cache — or "" when no
// cache dir is configured. It is the single source of truth for the path the
// refresh runner sets and the built-in shell exports, so both warm one cache.
func RepoCacheDir(cacheDir, repoName string) string {
	root := CacheRoot(cacheDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, RepoCacheName(repoName))
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
