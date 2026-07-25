package app

// The root extract helper revalidates requests and runs App.Extract's pipeline.
// Resolved credentials arrive in one stdin JSON value; EOF cancels helper and restic.
// Stdout NDJSON carries path-free errors and result paths echoed from the request.
// User cache sharing requires an owned real directory; otherwise no cache is used.
// Scaffolding is user-owned; failed staging returns best-effort to the user.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
	"github.com/alexzeitgeist/resticscope/internal/secrets"

	json "github.com/goccy/go-json"
)

// helperPayloadVersion rejects mismatched parent and sudo-helper wire formats.
// Version 3 made target and credentials backend-agnostic.
const helperPayloadVersion = 3

// helperPayload carries all pipeline inputs, including resolved credentials, in
// one stdin-only JSON value so the helper reads no config or secrets command.
type helperPayload struct {
	Version int            `json:"version"`
	Target  resticx.Target `json:"target"`
	Creds   resticx.Creds  `json:"creds"`
	Request ExtractRequest `json:"request"`

	// UnsafeSymlinks is the parent's validated [extract] unsafe_symlinks policy.
	UnsafeSymlinks string `json:"unsafe_symlinks"`
	// CacheDir enables user-owned cache sharing when the invoking user is known.
	CacheDir string `json:"cache_dir"`
	// TimeoutSeconds is the parent timeout; non-positive values rely on pipe closure.
	TimeoutSeconds int64 `json:"timeout_seconds"`
}

// Helper error codes preserve errors.Is sentinel behavior across the stdout
// protocol; uncategorized failures use a path-free generic event.
const (
	helperEventProgress = "progress"
	helperEventResult   = "result"
	helperEventError    = "error"

	helperCodeCanceled       = "canceled"
	helperCodeDeadline       = "deadline"
	helperCodeStagingExists  = "staging_exists"
	helperCodeFinalExists    = "final_exists"
	helperCodeInvalidRequest = "invalid_request"
	helperCodeMetadataNorm   = "metadata_normalization"
	helperCodeRenameFailed   = "rename_failed"
	helperCodeGeneric        = "generic"
)

// helperEvent is one NDJSON line on the helper's stdout.
type helperEvent struct {
	Kind     string           `json:"kind"`
	Progress *ExtractProgress `json:"progress,omitempty"`
	Result   *ExtractResult   `json:"result,omitempty"`
	Error    *helperError     `json:"error,omitempty"`
}

// helperError is a path-free terminal failure with enough staging state for the
// parent to offer cleanup.
type helperError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	StagingDir     string `json:"staging_dir,omitempty"`
	StagingCreated bool   `json:"staging_created"`
}

// ExtractHelperOpts supplies process facts and test seams to RunExtractHelper.
type ExtractHelperOpts struct {
	// Euid must be zero so restic restores ownership.
	Euid int

	// OwnerUID and OwnerGID receive scaffolding and failed staging ownership.
	// Negative values disable chown when the invoking user is unknown.
	OwnerUID int
	OwnerGID int

	// Clock times result.Elapsed. Required.
	Clock Clock

	// Restic overrides the driver; nil selects a redacting production client.
	Restic extractTreeDriver
}

// RunExtractHelper decodes a privileged payload, runs the pipeline, and emits
// events. Returned errors are path-free and are also emitted when possible.
func RunExtractHelper(ctx context.Context, in io.Reader, out io.Writer, opts ExtractHelperOpts) error {
	enc := json.NewEncoder(out)
	emitErr := func(code, msg, stagingDir string, stagingCreated bool) error {
		_ = enc.Encode(helperEvent{Kind: helperEventError, Error: &helperError{
			Code: code, Message: msg, StagingDir: stagingDir, StagingCreated: stagingCreated,
		}})
		return errors.New(msg)
	}

	if opts.Euid != 0 {
		return emitErr(helperCodeGeneric, "extract helper: must run as root (via sudo)", "", false)
	}

	var payload helperPayload
	if err := json.NewDecoder(in).Decode(&payload); err != nil {
		return emitErr(helperCodeGeneric, "extract helper: bad request payload", "", false)
	}
	if payload.Version != helperPayloadVersion {
		return emitErr(helperCodeGeneric, fmt.Sprintf("extract helper: payload version %d, want %d (binary mismatch?)", payload.Version, helperPayloadVersion), "", false)
	}

	// Stdin EOF cancels the run; the reader goroutine exits only when EOF arrives.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()

	// An empty repoCache forces no-cache when ownership cannot be returned safely.
	repoCache := ""
	if payload.CacheDir != "" && opts.OwnerUID >= 0 && opts.OwnerGID >= 0 {
		repoCache = resticx.RepoCacheDir(payload.CacheDir, payload.Target.Name)
	}

	drv := opts.Restic
	if drv == nil {
		drv = &resticx.Client{
			Runner: resticx.ExecRunner{},
			Stream: resticx.ExecRunner{},
			// Redact the password and credential environment values from the payload.
			CacheDir: payload.CacheDir,
			Redact:   helperRedactor(payload.Creds).Redact,
		}
	}

	result, runErr := runHelperExtract(runCtx, drv, payload, opts, repoCache, enc)
	if runErr != nil {
		// Return retained staging ownership so the non-root TUI can remove it.
		if result.StagingCreated {
			chownStagingForCleanup(result.StagingDir, opts.OwnerUID, opts.OwnerGID)
		}
		return emitErr(helperErrCode(runErr), runErr.Error(), result.StagingDir, result.StagingCreated)
	}
	if err := enc.Encode(helperEvent{Kind: helperEventResult, Result: &result}); err != nil {
		return fmt.Errorf("extract helper: emit result: %w", err)
	}
	return nil
}

// helperRedactor scrubs the restic password and every backend environment value.
func helperRedactor(creds resticx.Creds) *secrets.Redactor {
	values := make([]string, 0, len(creds.Env)+1)
	values = append(values, creds.ResticPassword)
	for _, v := range creds.Env {
		values = append(values, v)
	}
	return secrets.NewRedactor(values...)
}

// runHelperExtract revalidates the request and runs the shared pipeline. Its
// root-only seams preserve user scaffolding and cache ownership, emit NDJSON
// progress, and leave credential resolution and logging to the parent.
func runHelperExtract(ctx context.Context, drv extractTreeDriver, payload helperPayload, opts ExtractHelperOpts, repoCache string, enc *json.Encoder) (ExtractResult, error) {
	var result ExtractResult
	req := payload.Request

	// Revalidate the effective target and all safety gates at the root boundary.
	staging, final, err := PlanExtractPaths(config.Extract{}, req)
	if err != nil {
		return result, err
	}
	if err := checkExtractModeGate(req); err != nil {
		return result, err
	}
	if !liveExtractSupported {
		return result, errors.New("extract: not supported on this platform")
	}
	if err := FreshTargetCheck(staging, final); err != nil {
		return result, err
	}

	// Share only a prepared cache and return its ownership; otherwise use no-cache.
	if repoCache != "" {
		if !prepareHelperCache(repoCache, opts.OwnerUID, opts.OwnerGID) {
			repoCache = ""
		} else {
			defer chownCacheForOwner(repoCache, opts.OwnerUID, opts.OwnerGID)
		}
	}

	// Keep scaffolding user-owned while live staging stays root-owned. Progress
	// encode failures are deferred to the terminal result or error encode.
	return runExtractPipeline(ctx, req, staging, final, extractPipeline{
		drv:     drv,
		target:  payload.Target,
		creds:   payload.Creds,
		policy:  unsafeSymlinkPolicy(payload.UnsafeSymlinks),
		timeout: time.Duration(payload.TimeoutSeconds) * time.Second,
		clock:   opts.Clock,
		noCache: repoCache == "",
		mkdir:   func(dir string) error { return mkdirAllOwned(dir, opts.OwnerUID, opts.OwnerGID) },
		onProgress: func(p ExtractProgress) {
			_ = enc.Encode(helperEvent{Kind: helperEventProgress, Progress: &p})
		},
	})
}

// helperErrCode maps pipeline errors to sentinel-preserving wire codes.
func helperErrCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return helperCodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return helperCodeDeadline
	case errors.Is(err, ErrExtractStagingExists):
		return helperCodeStagingExists
	case errors.Is(err, ErrExtractFinalExists):
		return helperCodeFinalExists
	case errors.Is(err, ErrExtractInvalidRequest):
		return helperCodeInvalidRequest
	case errors.Is(err, ErrExtractMetadataNormalization):
		return helperCodeMetadataNorm
	case errors.Is(err, ErrExtractRenameFailed):
		return helperCodeRenameFailed
	default:
		return helperCodeGeneric
	}
}

// mkdirAllOwned creates missing directories owned by uid:gid without changing
// existing ancestors. Negative IDs use plain MkdirAll. Creation and Lchown are
// confined beneath the opened ancestor; Lchown does not follow a newly created
// directory swapped for a symlink. The ancestor path is not pinned before OpenRoot.
func mkdirAllOwned(dir string, uid, gid int) error {
	if uid < 0 || gid < 0 {
		return os.MkdirAll(dir, 0o700)
	}
	// Collect missing levels below the first non-followed existing ancestor.
	var missing []string
	cur := dir
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if len(missing) == 0 {
		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
		}
		return nil // dir already exists; nothing to create or own
	}
	// Create parent-first beneath the existing ancestor's os.Root.
	rt, err := os.OpenRoot(cur)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	for _, v := range slices.Backward(missing) {
		rel, rerr := filepath.Rel(cur, v)
		if rerr != nil {
			return rerr
		}
		if err := rt.Mkdir(rel, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // A racing creator retains ownership.
			}
			return err
		}
		if err := rt.Lchown(rel, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// prepareHelperCache enables cache sharing only when Lstat finds a real leaf
// directory owned by the invoking user. It rejects a pre-existing foreign or
// symlink leaf but does not pin the path against later replacement; ancestor
// symlinks remain allowed. Failure selects no-cache.
func prepareHelperCache(repoCache string, uid, gid int) bool {
	if err := mkdirAllOwned(repoCache, uid, gid); err != nil {
		return false
	}
	// Require an owned real directory rather than trusting a symlink accepted by mkdir.
	info, err := os.Lstat(repoCache)
	if err != nil || !info.Mode().IsDir() {
		return false
	}
	return ownedByUID(info, uid)
}

// chownCacheForOwner best-effort returns root-written cache entries to the user.
// Content-addressed tmp-and-rename writes make this safe with concurrent restic
// processes; a missed node can affect later caching, not extraction correctness.
func chownCacheForOwner(dir string, uid, gid int) {
	if dir == "" || uid < 0 || gid < 0 {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk: skip the unreadable entry, keep going
		}
		_ = os.Lchown(p, uid, gid) //nolint:gosec // G122 warns about symlink races in walk callbacks, but Lchown acts on the link itself and WalkDir does not descend through symlinks, so a swapped path cannot redirect the chown
		return nil
	})
}

// chownStagingForCleanup best-effort returns failed staging and directory u+rwx
// to the invoking user. Once staging is opened, os.Root confines descendant
// operations; it does not authenticate a path replaced before OpenRoot. Go
// 1.25.9+ addresses GO-2026-4864 symlink races within the opened root. Unfixed
// nodes may require sudo removal but cannot affect extract correctness.
func chownStagingForCleanup(staging string, uid, gid int) {
	if staging == "" || uid < 0 || gid < 0 {
		return
	}
	rt, err := os.OpenRoot(staging)
	if err != nil {
		return
	}
	defer func() { _ = rt.Close() }()
	// Make the root traversable before handing back descendants.
	_ = rt.Lchown(".", uid, gid)
	if fi, lerr := rt.Lstat("."); lerr == nil {
		_ = rt.Chmod(".", fi.Mode().Perm()|0o700)
	}
	chownStagingTree(rt, ".", uid, gid)
}

// chownStagingTree best-effort returns descendants without following links.
// Root-confined raw ReadDir supports non-UTF-8 Unix names that io/fs rejects;
// directories also receive owner rwx.
func chownStagingTree(rt *os.Root, dir string, uid, gid int) {
	f, err := rt.Open(dir)
	if err != nil {
		return
	}
	entries, _ := f.ReadDir(-1) // best-effort: act on whatever entries we could read
	_ = f.Close()
	for _, e := range entries {
		rel := filepath.Join(dir, e.Name())
		_ = rt.Lchown(rel, uid, gid)
		if e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				_ = rt.Chmod(rel, info.Mode().Perm()|0o700)
			}
			chownStagingTree(rt, rel, uid, gid)
		}
	}
}
