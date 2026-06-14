package app

// extract_helper.go is the root side of the privileged extract: the runtime
// behind the hidden `resticscope extract-helper` subcommand, which the TUI's
// non-root process launches via `sudo -n -- <self> extract-helper`. The helper
// re-asserts the boundary checks, then runs the literal same
// restore→normalize→publish pipeline as App.Extract (runExtractPipeline) —
// but as root, so restic applies the snapshot's file ownership (which it
// skips as non-root).
//
// Privilege-boundary contract:
//   - The request payload (repo target, resolved credentials, ExtractRequest,
//     extract config) arrives as ONE JSON value on stdin. stdin is the only
//     secret channel: nothing secret ever touches argv or the environment, and
//     sudo passes stdin through untouched. The parent resolved the credentials
//     with the user's own environment — the helper never runs secrets_command.
//   - Progress / result / error events leave as NDJSON on stdout. Error
//     messages are path-free (same construction as App.Extract); result events
//     carry paths, which is the parent's own request echoed back, never logged.
//   - Cancellation is stdin EOF: the parent closes the pipe (or dies) and the
//     helper cancels its context, which kills the restic child. The parent
//     cannot signal a root-owned process, so the pipe IS the kill switch.
//   - restic shares the invoking user's per-repo cache (the parent sends the
//     resticscope cache root): without it every index/tree-blob read is a
//     remote round trip and a privileged extract runs orders of magnitude
//     slower than a normal one. A root-run restic must neither duplicate the
//     cache under /root nor leave root-owned entries in the user's cache, so
//     the helper pre-creates the per-repo cache dir chain owned by the
//     invoking user and chowns every cache entry back after the run. That
//     chown-back is authorized by proof, not trust: cache_dir is user config,
//     so the share (and the re-own) only happens after the per-repo leaf is
//     verified to be a real directory owned by the invoking user
//     (prepareHelperCache). On any mismatch — no cache dir, unknown invoker,
//     file/symlink/foreign-owned leaf — the restore falls back to --no-cache.
//   - Directories the helper creates AROUND the extracted content (the target
//     scaffolding and mirror ancestors) are chown'd to the invoking user
//     (SUDO_UID/SUDO_GID, resolved by cmd) so later non-privileged extracts
//     can still merge into the same tree. Only the extracted nodes themselves
//     keep snapshot ownership — that is the point of the feature.
//   - On failure the retained staging tree is chown'd (and its dirs chmod'd
//     u+rwx) back to the invoking user, so the TUI's keep-or-delete prompt
//     keeps working: metadata fidelity is moot for a failed extract, but the
//     user must be able to delete what this run left behind.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"resticscope/internal/config"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"

	json "github.com/goccy/go-json"
)

// helperPayloadVersion guards the stdin wire shape. The parent and helper are
// the same binary in normal operation, but a stale installed binary under sudo
// must fail loudly rather than misread the request.
// Version 3: Target/Creds went backend-agnostic (repository string + env maps
// instead of S3 fields).
const helperPayloadVersion = 3

// helperPayload is the single JSON value the parent writes to the helper's
// stdin. It carries everything the pipeline needs so the helper reads no
// config file and runs no secrets_command. Creds contains the resolved repo
// secrets — stdin-only by design.
type helperPayload struct {
	Version int            `json:"version"`
	Target  resticx.Target `json:"target"`
	Creds   resticx.Creds  `json:"creds"`
	Request ExtractRequest `json:"request"`

	// UnsafeSymlinks is the parent's validated [extract] unsafe_symlinks policy.
	UnsafeSymlinks string `json:"unsafe_symlinks"`
	// CacheDir is the parent's resticscope cache root (Global.CacheDir). When
	// set and the invoking user is known, the helper's restic shares the
	// user's per-repo cache instead of running --no-cache; see runHelperExtract.
	CacheDir string `json:"cache_dir"`
	// TimeoutSeconds is the parent's extract_timeout; <=0 means no helper-side
	// deadline (the parent's pipe close still bounds the run).
	TimeoutSeconds int64 `json:"timeout_seconds"`
}

// Helper event kinds and error codes — the stdout wire vocabulary. Codes exist
// so the parent can reconstruct the app-layer sentinels (errors.Is must keep
// working across the process boundary); everything else rides "generic" with a
// path-free message.
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

// helperError is the terminal failure record. Message is path-free (the same
// error text App.Extract would have returned). StagingDir/StagingCreated let
// the parent surface keep-or-delete for output this run owns.
type helperError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	StagingDir     string `json:"staging_dir,omitempty"`
	StagingCreated bool   `json:"staging_created"`
}

// ExtractHelperOpts are the injected process facts and seams for
// RunExtractHelper. cmd fills them from the real process; tests inject fakes.
type ExtractHelperOpts struct {
	// Euid is the effective uid the helper runs as. Anything but 0 is refused:
	// without root, restic skips ownership and the helper would silently
	// deliver exactly what the user asked it to avoid.
	Euid int

	// OwnerUID / OwnerGID identify the invoking user (cmd resolves SUDO_UID /
	// SUDO_GID). Scaffolding dirs and failed staging are chown'd to them. Either
	// being negative disables all chowns (unknown invoker — dirs stay root's).
	OwnerUID int
	OwnerGID int

	// Clock times result.Elapsed. Required.
	Clock Clock

	// Restic overrides the restore driver in tests. nil selects the production
	// resticx.Client (ExecRunner, secrets-redacting stderr scrubber).
	Restic extractTreeDriver
}

// RunExtractHelper is the entry point behind `resticscope extract-helper`. It
// decodes the payload from in, runs the pipeline, and emits events on out. The
// returned error (always also emitted as an error event when possible) is
// path-free; cmd prints it to stderr and exits non-zero.
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

	// stdin EOF is the cancellation channel: the parent closes its end (or
	// exits) and the run context falls. The goroutine exits at that same EOF,
	// so it never outlives the call (rule 12: no goroutine without
	// cancellation — the pipe is its cancellation).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()

	// repoCache is the user's per-repo restic cache the restore may share; ""
	// forces --no-cache (no cache dir from the parent, or an unknown invoker —
	// nobody to hand root-written cache entries back to).
	repoCache := ""
	if payload.CacheDir != "" && opts.OwnerUID >= 0 && opts.OwnerGID >= 0 {
		repoCache = resticx.RepoCacheDir(payload.CacheDir, payload.Target.Name)
	}

	drv := opts.Restic
	if drv == nil {
		drv = &resticx.Client{
			Runner: resticx.ExecRunner{},
			Stream: resticx.ExecRunner{},
			// CacheDir points restic at the user's cache when the share is
			// permitted; when the pipeline falls back to --no-cache restic
			// ignores it entirely. The helper has no secrets.Store, so this
			// Redactor over the payload's password and credential env values is
			// its whole redaction surface (rule 8) — they are the only secrets
			// that exist in this process.
			CacheDir: payload.CacheDir,
			Redact:   helperRedactor(payload.Creds).Redact,
		}
	}

	result, runErr := runHelperExtract(runCtx, drv, payload, opts, repoCache, enc)
	if runErr != nil {
		// Hand the retained staging back to the invoking user so keep-or-delete
		// in the (non-root) TUI still works on a tree restic populated as root.
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

// helperRedactor builds the helper's secrets scrubber from the payload's
// resolved credentials: the restic password plus every backend env value.
func helperRedactor(creds resticx.Creds) *secrets.Redactor {
	values := make([]string, 0, len(creds.Env)+1)
	values = append(values, creds.ResticPassword)
	for _, v := range creds.Env {
		values = append(values, v)
	}
	return secrets.NewRedactor(values...)
}

// runHelperExtract re-asserts the boundary checks (regardless of what the
// parent verified) and then runs the same shared pipeline as App.Extract.
// Every deliberate root-side delta is an extractPipeline seam: scaffolding
// dirs are created owned by the invoking user, restic shares the user's repo
// cache (repoCache; "" falls back to --no-cache), progress leaves as NDJSON
// events — and there is no cred resolution or logging (the parent owns both).
func runHelperExtract(ctx context.Context, drv extractTreeDriver, payload helperPayload, opts ExtractHelperOpts, repoCache string, enc *json.Encoder) (ExtractResult, error) {
	var result ExtractResult
	req := payload.Request

	// Validate request, file-type gate, platform gate, fresh-target check —
	// re-asserted at the privilege boundary. The parent set req.TargetRoot to
	// the effective absolute root, so the config side of PlanExtractPaths can
	// stay zero here.
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

	// Shared cache: prepare the per-repo cache dir (created user-owned, and —
	// the privilege-boundary check — proven to be a real directory owned by
	// the invoking user before root writes into it or re-owns it), and hand
	// every cache entry back to the user after the run — restic writes each
	// cache miss as root, and a root-owned 0700 fanout subdir would refuse
	// the user's own later cache writes. The cache is an optimization:
	// failing to prepare it degrades to --no-cache, never fails the extract.
	// The deferred chown-back covers success, failure, and cancellation alike.
	if repoCache != "" {
		if !prepareHelperCache(repoCache, opts.OwnerUID, opts.OwnerGID) {
			repoCache = ""
		} else {
			defer chownCacheForOwner(repoCache, opts.OwnerUID, opts.OwnerGID)
		}
	}

	// Steps 5–9, shared with App.Extract. mkdirAllOwned keeps every
	// scaffolding dir (staging parent, mirror ancestors) owned by the invoking
	// user, so a later non-privileged extract can still merge into the same
	// tree and the only root-owned nodes that land in the mirror are the
	// extracted ones; staging itself stays root-owned while the run is live
	// (removed on success, chown'd back to the user on failure by the caller).
	// The parent enforces the same timeout bound and the pipe close backs both
	// up. Progress-encode failures are swallowed: a wedged stdout surfaces
	// soon enough via the result/error encode.
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

// helperErrCode maps a pipeline error to its wire code so the parent can
// reconstruct the matching sentinel (errors.Is across the process boundary).
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

// mkdirAllOwned is MkdirAll with ownership: every directory it CREATES is
// chown'd to uid:gid, while pre-existing ancestors are left untouched (same
// policy as App.Extract — the user owns their dirs' metadata). A negative uid
// or gid disables the chown (unknown invoker), leaving plain MkdirAll
// semantics.
//
// Each created level is chowned to the invoking user, making its parent
// user-writable for the next — so a limited-sudo invoker could swap a fresh dir
// for a symlink between the Mkdir and the chown. The create+chown is confined to
// the first existing ancestor via *os.Root, and the chown is an Lchown (not
// Chown): a swapped-in symlink is chowned in place rather than followed off-tree,
// closing the race by construction (no dependency on the Chmod-race fix).
func mkdirAllOwned(dir string, uid, gid int) error {
	if uid < 0 || gid < 0 {
		return os.MkdirAll(dir, 0o700)
	}
	// Walk up (non-following Lstat) to the first existing ancestor, collecting
	// the missing levels below it.
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
		return nil // dir already exists; nothing to create or own
	}
	// cur is the first existing ancestor: confine the downward create+chown to
	// it. missing is parent-first from the back, so each rt.Mkdir's parent was
	// created in a prior iteration.
	rt, err := os.OpenRoot(cur)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	for i := len(missing) - 1; i >= 0; i-- {
		rel, rerr := filepath.Rel(cur, missing[i])
		if rerr != nil {
			return rerr
		}
		if err := rt.Mkdir(rel, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // racing creator; ownership is theirs
			}
			return err
		}
		if err := rt.Lchown(rel, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// prepareHelperCache makes the per-repo cache dir safe for a root-run restic
// to share: every missing level is created owned by the invoking user, and
// the final path is verified to be a real directory (no file, no symlink)
// owned by that user. The ownership proof is what authorizes the later
// recursive chown-back — cache_dir is user CONFIG, not a vetted path, so a
// cache root pointed at a shared or system location must never have its
// pre-existing tree re-owned to the invoker; and a symlink leaf would both
// let restic write through to an unexpected target and escape the chown walk
// (WalkDir does not follow a symlink root). Ancestor symlinks (a linked
// ~/.cache is a normal setup) stay allowed: they do not widen what the leaf
// walk re-owns. false means run --no-cache instead.
func prepareHelperCache(repoCache string, uid, gid int) bool {
	if err := mkdirAllOwned(repoCache, uid, gid); err != nil {
		return false
	}
	// Re-Lstat rather than trusting mkdirAllOwned: its target-exists check is
	// deliberately lenient (a pre-existing leaf of any type counts as done).
	info, err := os.Lstat(repoCache)
	if err != nil || !info.Mode().IsDir() {
		return false
	}
	return ownedByUID(info, uid)
}

// chownCacheForOwner hands the shared per-repo restic cache back to the
// invoking user after a privileged run. restic wrote any cache miss as the
// process user (root here): index files, metadata packs, and possibly new
// 0700 fanout subdirs the user's own restic could no longer write into. Cache
// entries are content-addressed and written via tmp+rename, so re-owning them
// is safe against concurrent non-root restics. Best-effort: a node left
// root-owned costs at worst a cache write error in a later non-elevated run,
// never extract correctness.
func chownCacheForOwner(dir string, uid, gid int) {
	if dir == "" || uid < 0 || gid < 0 {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk: skip the unreadable entry, keep going
		}
		_ = os.Lchown(p, uid, gid)
		return nil
	})
}

// chownStagingForCleanup hands a failed run's staging tree to the invoking user
// so the non-root TUI can honor keep-or-delete. Ownership fidelity is moot for a
// failed extract, so directories additionally get u+rwx (RemoveAll needs to
// descend and unlink). The chown/chmod is confined to the staging subtree via an
// *os.Root: this runs as root over restic-restored, attacker-named content, and
// os.Chmod follows a final symlink (Lchown does not), so without confinement a
// swapped-in escaping symlink could redirect a root chmod off-tree. os.Root
// rejects escapes and (go1.25.9+, GO-2026-4864) closes the dir→symlink chmod
// race. Best-effort: a node it can't fix — or a root it can't open — leaves at
// worst a tree the user must sudo-remove, which the delete error says plainly.
func chownStagingForCleanup(staging string, uid, gid int) {
	if staging == "" || uid < 0 || gid < 0 {
		return
	}
	rt, err := os.OpenRoot(staging)
	if err != nil {
		return
	}
	defer func() { _ = rt.Close() }()
	// Hand the staging root back first and ensure it carries owner rwx so its
	// children can be listed and unlinked, then recurse into it.
	_ = rt.Lchown(".", uid, gid)
	if fi, lerr := rt.Lstat("."); lerr == nil {
		_ = rt.Chmod(".", fi.Mode().Perm()|0o700)
	}
	chownStagingTree(rt, ".", uid, gid)
}

// chownStagingTree recurses dir (relative to rt) and hands every descendant to
// the invoking user. It walks via rt.Open + (*os.File).ReadDir rather than
// fs.WalkDir(rt.FS()) because io/fs rejects non-UTF-8 paths: that walk would
// refuse to recurse into a directory whose restored name is not valid UTF-8
// (legal in Unix backups) and strand its children. The raw-byte readdir has no
// such limit and still opens through the root. Lchown is non-following; dirs also
// get owner rwx. Best-effort: an unreadable or unfixable node is skipped.
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
