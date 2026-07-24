// Package resticx executes restic as the application's external-process boundary.
// It constructs restricted environments, transports repository passwords outside
// argv, parses JSON defensively, and classifies exit failures. Repository and
// credential coordinates enter as plain structs to preserve package layering.
package resticx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

const defaultTimeout = 2 * time.Minute

// Target contains backend-agnostic restic repository coordinates. Env contains
// only non-secret values; secret environment values belong in Creds.
type Target struct {
	Name    string            // repo name; used for the per-repo restic cache subdir
	Repo    string            // RESTIC_REPOSITORY, e.g. "s3:https://host/bucket" or "sftp:user@host:/srv/repo"
	Options map[string]string // restic -o key=value backend options
	Env     map[string]string // non-secret backend env vars (e.g. AWS_DEFAULT_REGION)
}

// Creds contains resolved backend environment secrets and the repository
// password. The password is delivered out of band on fd 3, never on argv.
type Creds struct {
	Env            map[string]string
	ResticPassword string
}

// Runner executes one restic invocation and separates parseable stdout from
// redactable stderr.
type Runner interface {
	Run(ctx context.Context, env []string, password string, args ...string) (stdout, stderr []byte, err error)
}

// StreamRunner executes restic with streaming stdout. onStdout runs on the
// caller's goroutine, and RunStream returns after the callback and process exit.
type StreamRunner interface {
	RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) (stderr []byte, err error)
}

// PatternStreamRunner optionally supplies a pattern-file payload on fd 4.
// ExtractTree requires it only for multi-include restores.
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

// ErrNoRunner indicates that a buffered call has no configured Runner.
var ErrNoRunner = errors.New("resticx: no runner configured (set Client.Runner)")

// Snapshots lists the repository's snapshots.
func (c *Client) Snapshots(ctx context.Context, t Target, creds Creds) ([]model.Snapshot, error) {
	// Lockless reads support repositories that reject lock writes.
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

// CatConfig probes repository access by decrypting its config without a lock. It
// returns classified backend, repository, and password failures and discards the
// decrypted repository metadata rather than treating it as secret-free.
func (c *Client) CatConfig(ctx context.Context, t Target, creds Creds) error {
	_, err := c.runOp(ctx, t, creds, "cat", "--no-lock", "cat", "config")
	return err
}

// Version returns restic's version string (e.g. "0.18.1"). It parses the plain
// `restic version` output, which is stable across the supported range.
func (c *Client) Version(ctx context.Context) (string, error) {
	if c.Runner == nil {
		return "", ErrNoRunner
	}
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

// runOp executes a buffered operation with the client's timeout, backend options,
// and restricted environment. It classifies process failures before returning.
func (c *Client) runOp(ctx context.Context, t Target, creds Creds, op string, args ...string) ([]byte, error) {
	if c.Runner == nil {
		return nil, ErrNoRunner
	}
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

// prependBackendOpts adds backend options in deterministic key order.
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

// buildEnv constructs a restricted restic environment that references the fd-3
// password without containing it.
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

// BackendEnviron returns sorted backend KEY=value entries, with Creds.Env taking
// precedence over Target.Env; repository shells use the same environment. It
// rejects invalid names and the explicit reserved set at this privileged-process
// boundary: PATH, HOME, restic repository/password/cache selectors, and LD_/DYLD_
// loader variables.
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

// sortedKeys returns deterministic map keys.
func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

func minimalEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
}

func (c *Client) repoCacheDir(t Target) string {
	return RepoCacheDir(c.CacheDir, t.Name)
}

// RepoCacheDir returns a repository's cache directory, or "" when caching is
// disabled. Refresh and shell processes share this path.
func RepoCacheDir(cacheDir, repoName string) string {
	root := CacheRoot(cacheDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, RepoCacheName(repoName))
}

// CacheRoot returns the parent of per-repository restic caches, or "" when
// caching is disabled. The cache-prune command scans this layout.
func CacheRoot(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "restic-cache")
}

// RepoCacheName returns the sanitized path element shared by restic and cache
// pruning for one repository.
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
