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
//   - restic runs with --no-cache: a root-run restic must neither duplicate
//     the user's cache under /root nor leave root-owned files in it.
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
const helperPayloadVersion = 1

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

	helperCodeCanceled      = "canceled"
	helperCodeDeadline      = "deadline"
	helperCodeStagingExists = "staging_exists"
	helperCodeFinalExists   = "final_exists"
	helperCodeGeneric       = "generic"
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
	emitErr := func(code, msg string, stagingDir string, stagingCreated bool) error {
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

	drv := opts.Restic
	if drv == nil {
		drv = &resticx.Client{
			Runner: resticx.ExecRunner{},
			Stream: resticx.ExecRunner{},
			// No CacheDir: the restore runs --no-cache, so restic touches no
			// cache at all (no /root duplicate, no root-owned user-cache files).
			// The helper has no secrets.Store, so this Redactor over the
			// payload's three values is its whole redaction surface (rule 8) —
			// they are the only secrets that exist in this process.
			Redact: secrets.NewRedactor(payload.Creds.ResticPassword, payload.Creds.SecretKey, payload.Creds.AccessKey).Redact,
		}
	}

	result, runErr := runHelperExtract(runCtx, drv, payload, opts, enc)
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

// runHelperExtract re-asserts the boundary checks (regardless of what the
// parent verified) and then runs the same shared pipeline as App.Extract.
// Every deliberate root-side delta is an extractPipeline seam: scaffolding
// dirs are created owned by the invoking user, restic runs --no-cache,
// progress leaves as NDJSON events — and there is no cred resolution or
// logging (the parent owns both).
func runHelperExtract(ctx context.Context, drv extractTreeDriver, payload helperPayload, opts ExtractHelperOpts, enc *json.Encoder) (ExtractResult, error) {
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
		noCache: true,
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
	default:
		return helperCodeGeneric
	}
}

// mkdirAllOwned is MkdirAll with ownership: every directory it CREATES is
// chown'd to uid:gid, while pre-existing ancestors are left untouched (same
// policy as App.Extract — the user owns their dirs' metadata). A negative uid
// or gid disables the chown (unknown invoker), leaving plain MkdirAll
// semantics.
func mkdirAllOwned(dir string, uid, gid int) error {
	if uid < 0 || gid < 0 {
		return os.MkdirAll(dir, 0o700)
	}
	// Walk up to the first existing ancestor, then create downward, chowning
	// each new level.
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
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // racing creator; ownership is theirs
			}
			return err
		}
		if err := os.Chown(missing[i], uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// chownStagingForCleanup hands a failed run's staging tree to the invoking
// user so the non-root TUI can honor keep-or-delete. Ownership fidelity is
// moot for a failed extract, so directories additionally get u+rwx (RemoveAll
// needs to descend and unlink). Lchown never follows symlinks, so a restored
// link can't redirect the chown onto the live filesystem. Best-effort by
// design: a node it cannot fix leaves at worst a staging dir the user must
// sudo-remove, which the delete error will say plainly.
func chownStagingForCleanup(staging string, uid, gid int) {
	if staging == "" || uid < 0 || gid < 0 {
		return
	}
	_ = filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking what we can
		}
		_ = os.Lchown(p, uid, gid)
		if d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}
