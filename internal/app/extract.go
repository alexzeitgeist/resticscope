package app

// extract.go is the app-layer orchestrator for the extract feature: the single
// entry point (App.Extract) that turns a fully-explicit ExtractRequest into a
// safe restic invocation, normalizes the staging tree's metadata, and atomically
// renames staging → final on clean completion. It owns the safety policy
// (fresh-target check, file-type gate, metadata normalization), the
// staging-then-rename machinery, and the privacy boundary (every returned error
// is path-free). resticx owns the argv-level §5 invariants; this layer owns the
// filesystem-level ones and never lets a source/staging/final path reach a log
// or a returned error. It writes nothing to Cache or RepoState.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"resticscope/internal/config"
	"resticscope/internal/model"
	"resticscope/internal/resticx"
	"resticscope/internal/secrets"
)

// ExtractMode selects the underlying restic shape. It is source-driven — the TUI
// derives it from the selected BrowseEntry, the app layer never toggles it.
type ExtractMode int

const (
	// ExtractFile restores one regular file (restic restore --include), preserving
	// the snapshot's mode/mtime/owner/xattrs — no longer a byte-only dump. The
	// file lands flattened or nested per ExtractRequest.Nested.
	ExtractFile ExtractMode = iota
	// ExtractDirectoryTree restores a whole subtree (restic restore), and is also
	// the future full-snapshot path.
	ExtractDirectoryTree
)

// unsafeSymlinkPolicy selects what the staging metadata pass does with a symlink
// whose target is absolute or escapes the extracted tree (so it would alias the
// live filesystem once the fragment lands in a scratch dir). It is the validated
// [extract] unsafe_symlinks config value. The zero value ("") and any unknown
// string behave as keep (fail-safe) — see applyUnsafeSymlinkPolicy. The type is
// defined here (not in extract_metadata.go) so the linux/darwin normalizer and
// the other-platform stub share one definition.
type unsafeSymlinkPolicy string

const (
	unsafeSymlinkKeep        unsafeSymlinkPolicy = "keep"        // leave verbatim + warn (restic-faithful)
	unsafeSymlinkSkip        unsafeSymlinkPolicy = "skip"        // remove from the output
	unsafeSymlinkPlaceholder unsafeSymlinkPolicy = "placeholder" // replace with an inert text file recording the target
)

// ExtractRequest is the explicit contract between the TUI / future CLI and the
// app layer. Every field is provided by the caller; App.Extract inspects no
// BrowseEntry. The app layer re-asserts the slug rule and file-type gate before
// it touches the filesystem, so a malformed or mis-routed request is refused at
// the boundary regardless of which caller built it.
type ExtractRequest struct {
	// Repo is the logical repository name (display + logging). Only its slugged
	// form reaches the filesystem.
	Repo string

	// SnapshotID is the verified concrete hex snapshot ID. Never "latest".
	SnapshotID string

	// SnapshotShort is the first 8 hex chars (display + target-name). Caller
	// computes it; the app layer asserts ^[0-9a-f]{8}$ and never derives it.
	SnapshotShort string

	// Source is the cleaned, rooted source path inside the snapshot. "/" is
	// reserved for the future full-snapshot path; browse-originated requests carry
	// a non-root path.
	Source string

	// SourceName is the sanitized basename forming the per-op subdir. The app
	// layer re-asserts SourceName == SanitizeExtractSlug(path.Base(Source)) (or ==
	// SnapshotShort for the root source) so a caller-supplied slug cannot be
	// attached to the wrong source.
	SourceName string

	Mode ExtractMode

	// WasRegularFile mirrors the upstream BrowseEntry.Type == "file" check at the
	// app boundary. Required true for ExtractFile, false for ExtractDirectoryTree —
	// symlinks/devices/fifos/sockets are rejected here. A regular file now routes
	// through restic restore (with --include), not a dump.
	WasRegularFile bool

	// Nested chooses the file layout and is IGNORED for directories. It is phrased
	// so the zero value is the default: false = flattened (the file lands at
	// <final>/<name>, the historical dump location, now metadata-preserving),
	// true = nested (the file lands at <final>/<original/path>). Named Nested
	// rather than Flatten precisely so a builder that omits it gets the flattened
	// default — ExtractRequest is an explicit contract where the zero value matters.
	Nested bool

	// TargetRoot optionally overrides cfg.Extract.TargetRoot for this one call.
	// Empty means use the configured root; non-empty must be absolute.
	TargetRoot string

	// DryRun toggles restic restore --dry-run -vv. Must be false for ExtractFile:
	// the file flow has no restic dry-run (the file review is TUI-side), and
	// rejecting it on every platform also closes a hole — an allowed file dry-run
	// would invoke restic with a non-Windows-safe --include pattern.
	DryRun bool
}

// ExtractProgress is the flattened progress the orchestrator hands to the TUI.
// resticx events never reach the TUI directly.
type ExtractProgress struct {
	BytesDone        int64
	BytesTotal       int64 // 0 until restic reports it
	FilesDone        int
	FilesTotal       int
	SecondsElapsed   float64
	SecondsRemaining float64
}

// ExtractResult is returned from App.Extract. FinalDir is set only after a clean
// non-dry rename. StagingDir/StagingCreated are set the instant the staging dir
// is created and are returned even alongside a later error, so the TUI can offer
// keep-or-delete for output this run owns. DryRunPreview is in-memory only.
type ExtractResult struct {
	Files   int
	Dirs    int
	Bytes   int64
	Elapsed time.Duration

	// UnsafeSymlinks counts symlinks whose target is absolute or escapes the
	// extracted tree; Other counts device/fifo/socket nodes left in place. Both
	// come from the normalizer walk (live tree extract only). UnsafeSymlinkPolicy
	// records which [extract] unsafe_symlinks policy applied, so the TUI can phrase
	// the warning correctly (kept / removed / replaced).
	UnsafeSymlinks      int
	Other               int
	UnsafeSymlinkPolicy string

	FinalDir string

	StagingDir     string
	StagingCreated bool

	DryRunPreview []ExtractPreviewItem
}

// ExtractPreviewItem is one row of a directory dry-run preview.
type ExtractPreviewItem struct {
	Action model.RestoreAction // restored / metadata / skipped, typed at the resticx boundary
	Item   string              // relative path under the subtree root
	Size   int64
}

// Extract sentinels. All are path-free by construction; the messages name a
// field or phase, never a path or a field value.
var (
	// ErrExtractInvalidRequest is returned when PlanExtractPaths or the file-type
	// gate rejects the request. The wrapped message names the offending field.
	ErrExtractInvalidRequest = errors.New("extract: invalid request")
	// ErrExtractStagingExists is returned when the planned staging dir already
	// exists (fresh-target check, or the exclusive mkdir saw it).
	ErrExtractStagingExists = errors.New("extract: staging directory already exists")
	// ErrExtractFinalExists is returned when the planned final dir already exists
	// (fresh-target check, or the pre-rename re-check).
	ErrExtractFinalExists = errors.New("extract: target directory already exists")
	// ErrExtractMetadataNormalization is returned when the post-restore staging
	// metadata pass fails or finds metadata that cannot be safely normalized.
	// Staging is left in place and no rename is attempted.
	ErrExtractMetadataNormalization = errors.New("extract: staging metadata normalization failed")
	// ErrExtractRenameFailed is returned when os.Rename(staging, final) fails on
	// the clean-completion path (e.g. EXDEV — which should never happen since they
	// share a parent).
	ErrExtractRenameFailed = errors.New("extract: staging rename failed")
)

// invalidExtractRequest wraps ErrExtractInvalidRequest with a field name. The
// name is a fixed token (never a request value) so the message stays path-free.
func invalidExtractRequest(field string) error {
	return fmt.Errorf("%w: %s", ErrExtractInvalidRequest, field)
}

// extractSnapshotShortRe is restic's short-ID shape: exactly 8 lowercase hex.
var extractSnapshotShortRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// extractSnapshotIDRe is restic's full snapshot ID: a SHA-256 digest, exactly 64
// lowercase hex. The app layer asserts this at the boundary (mirroring resticx's
// assertCleanSnapshotID) so a malformed ID dies before any staging side effect,
// not after resticx rejects it at dispatch.
var extractSnapshotIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SanitizeExtractSlug keeps [a-zA-Z0-9._-], collapses any run of other bytes to a
// single "-", trims leading "-"/"." (so no dotfile or "-leading" path element is
// produced), and caps the result at 64 bytes. An empty result is an error. It is
// exported so the TUI (layer 3) builds SourceName with the exact rule
// PlanExtractPaths re-asserts, eliminating a byte-for-byte duplicate.
func SanitizeExtractSlug(s string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.TrimLeft(b.String(), "-.")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		return "", errors.New("slug is empty")
	}
	return out, nil
}

// PlanExtractPaths derives the staging and final directory paths from the request
// and config. It is exported for testing, is deterministic, and touches no
// filesystem. It also re-asserts every request invariant the app layer owns;
// failure returns a path-free ErrExtractInvalidRequest naming the bad field.
func PlanExtractPaths(cfg config.Extract, req ExtractRequest) (staging, final string, err error) {
	if req.Repo == "" {
		return "", "", invalidExtractRequest("repo")
	}
	repoSlug, err := SanitizeExtractSlug(req.Repo)
	if err != nil {
		return "", "", invalidExtractRequest("repo")
	}

	if !extractSnapshotShortRe.MatchString(req.SnapshotShort) {
		return "", "", invalidExtractRequest("snapshot_short")
	}
	if !extractSnapshotIDRe.MatchString(req.SnapshotID) {
		return "", "", invalidExtractRequest("snapshot_id")
	}
	// Bind the display/target short to the concrete ID so the output directory can
	// never be named for a different snapshot than the one actually extracted.
	if req.SnapshotShort != req.SnapshotID[:8] {
		return "", "", invalidExtractRequest("snapshot_short")
	}

	if req.Source == "" {
		return "", "", invalidExtractRequest("source")
	}
	// Refuse an unclean or NUL-bearing source at the boundary — before any hash,
	// staging dir, or restic spawn — so a malformed request cannot leave staging
	// behind or lean on resticx to reject it after filesystem side effects. This
	// asserts the same model.CleanBrowsePath contract resticx checks at dispatch.
	if strings.ContainsRune(req.Source, '\x00') || req.Source != model.CleanBrowsePath(req.Source) {
		return "", "", invalidExtractRequest("source")
	}

	// The app layer must not merely trust a caller-supplied safe slug: it must
	// belong to the selected source.
	var wantName string
	if req.Source == "/" {
		wantName = req.SnapshotShort
	} else {
		base, berr := SanitizeExtractSlug(path.Base(req.Source))
		if berr != nil {
			return "", "", invalidExtractRequest("source")
		}
		wantName = base
	}

	if req.SourceName == "" ||
		strings.ContainsRune(req.SourceName, '/') ||
		strings.ContainsRune(req.SourceName, filepath.Separator) ||
		strings.HasPrefix(req.SourceName, ".") ||
		req.SourceName != wantName {
		return "", "", invalidExtractRequest("source_name")
	}

	targetRoot := cfg.TargetRoot
	if req.TargetRoot != "" {
		if !filepath.IsAbs(req.TargetRoot) {
			return "", "", invalidExtractRequest("target_root")
		}
		targetRoot = req.TargetRoot
	}
	if targetRoot == "" || !filepath.IsAbs(targetRoot) {
		return "", "", invalidExtractRequest("target_root")
	}

	sum := sha256.Sum256([]byte(req.Source))
	hash := hex.EncodeToString(sum[:])[:8]
	subdir := req.SnapshotShort + "-" + req.SourceName + "-" + hash

	repoDir := filepath.Join(targetRoot, repoSlug)
	final = filepath.Join(repoDir, subdir)
	staging = filepath.Join(repoDir, ".resticscope-staging-"+subdir)
	return staging, final, nil
}

// checkExtractModeGate is the file-type gate (defense in depth). It runs before
// any restic spawn, mkdir, or fresh-target check, so non-regular sources and
// mode/file-type mismatches are refused at the boundary.
func checkExtractModeGate(req ExtractRequest) error {
	switch req.Mode {
	case ExtractFile:
		if !req.WasRegularFile {
			return invalidExtractRequest("mode")
		}
		if req.Source == "" || req.Source == "/" {
			return invalidExtractRequest("source")
		}
		if req.DryRun {
			// Files have no dry-run flow (the review is TUI-side). Rejecting it on
			// every platform also closes a hole: an allowed file dry-run would invoke
			// restic with --include <literalIncludePattern(...)>, whose backslash
			// escaping is not Windows-safe.
			return invalidExtractRequest("dry_run")
		}
		return nil
	case ExtractDirectoryTree:
		if req.WasRegularFile {
			// A directory request must not carry a regular-file source; restic
			// restore <snap>:<file> is not a valid whole-subtree shape.
			return invalidExtractRequest("mode")
		}
		return nil
	default:
		return invalidExtractRequest("mode")
	}
}

// Extract is the single entry point. It validates the request, gates on file
// type, resolves credentials, refuses an existing target, creates the staging
// dir, dispatches to the right resticx wrapper under a per-op timeout,
// normalizes live tree-extract metadata, and renames staging → final on clean
// completion. Every returned error is path-free. onProgress is best-effort and
// nil-tolerant; it is called from the resticx goroutine.
func (a *App) Extract(ctx context.Context, req ExtractRequest, onProgress func(ExtractProgress)) (ExtractResult, error) {
	var result ExtractResult

	// 1. Validate request (also derives the paths).
	staging, final, err := PlanExtractPaths(a.Cfg.Extract, req)
	if err != nil {
		return result, err
	}

	// 2. File-type gate — before any spawn, mkdir, or fresh-target check.
	if err := checkExtractModeGate(req); err != nil {
		return result, err
	}

	// 2b. Unsupported-platform preflight. The publish path runs the post-restore
	// metadata normalizer, which is validated only on linux/darwin, and the file
	// path's --include escaping is not Windows-safe — so refuse a live extract on
	// an unsupported platform BEFORE cred resolution / staging / restic, never
	// spawning restic with a non-literal include. A dry-run is allowed through:
	// the only dry-run that reaches here is a directory dry-run (file dry-run is
	// gate-rejected above), it carries no --include, and it returns before staging
	// / normalize, so it is Windows-safe and works everywhere.
	if !req.DryRun && !liveExtractSupported {
		return result, errors.New("extract: not supported on this platform")
	}

	// 3. Cred-resolution preamble (same path as ShellSession / BrowseSession).
	r, ok := a.repo(req.Repo)
	if !ok {
		return result, fmt.Errorf("extract: unknown repo %q", req.Repo)
	}
	if _, ok := a.Cfg.Credential(r.Credential); !ok { // unreachable after config validation, but stay defensive
		return result, fmt.Errorf("extract: credential %q not found", r.Credential)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return result, err
	}

	// 4. Fresh-target check (mandatory for both dry-runs and real runs).
	if err := freshTargetCheck(staging, final); err != nil {
		return result, err
	}

	// startedAt anchors result.Elapsed against the injected clock. Mtimes are no
	// longer reset, so this is its only use.
	startedAt := a.Clock.Now()

	// 5. Create the parent and staging dir (real runs only). A pre-existing
	// parent is left untouched — the user owns its policy; only the new staging
	// (and, via rename, final) dirs are 0700.
	if !req.DryRun {
		if err := os.MkdirAll(filepath.Dir(staging), 0o700); err != nil {
			return result, pathFreeExtractErr("create target parent", err)
		}
		if err := os.Mkdir(staging, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				return result, ErrExtractStagingExists
			}
			return result, pathFreeExtractErr("create staging", err)
		}
		result.StagingDir = staging
		result.StagingCreated = true
	}

	// 6. Per-op timeout (wraps both dry-runs and real runs).
	runCtx, cancel := context.WithTimeout(ctx, a.Cfg.Extract.ExtractTimeout.Std())
	defer cancel()

	// 7. Mode dispatch.
	if dispErr := a.dispatchExtract(runCtx, r, material, req, staging, &result, onProgress); dispErr != nil {
		// Prefer the local timeout/cancel verdict so the app boundary surfaces a
		// context error regardless of how resticx classified the interruption.
		if ce := runCtx.Err(); ce != nil {
			return result, fmt.Errorf("extract: %w", ce)
		}
		return result, fmt.Errorf("extract: %w", dispErr)
	}

	// 8. Metadata normalization gate (every live restore — file and directory —
	// since both now route through restic restore into staging). A single
	// flattened file walks to Files=1, Dirs=0; a nested file adds its reconstructed
	// parent dirs to Dirs. A dry-run returns before this (no staging exists).
	if !req.DryRun {
		policy := unsafeSymlinkPolicy(a.Cfg.Extract.UnsafeSymlinks)
		counts, nerr := normalizeExtractTreeMetadata(runCtx, staging, policy)
		if nerr != nil {
			return result, nerr
		}
		// The normalizer walk is authoritative for the live file/dir split, which
		// restic's summary cannot give (total_files folds dirs in).
		result.Files = counts.Files
		result.Dirs = counts.Dirs
		result.UnsafeSymlinks = counts.UnsafeSymlinks
		result.Other = counts.Other
		result.UnsafeSymlinkPolicy = a.Cfg.Extract.UnsafeSymlinks
	}

	// A dry-run never created a staging dir and never renames.
	if req.DryRun {
		return result, nil
	}

	// 9. Rename on clean completion. Re-check final immediately before the rename
	// rather than trusting os.Rename's behavior for an existing directory. Lstat
	// (not Stat) so a dangling symlink occupying the path is treated as present.
	if _, serr := os.Lstat(final); serr == nil {
		return result, ErrExtractFinalExists
	}
	if err := os.Rename(staging, final); err != nil {
		return result, fmt.Errorf("%w: %v", ErrExtractRenameFailed, pathFreeCause(err))
	}
	result.FinalDir = final

	// Total wall-clock for the operation, measured against the injected clock from
	// startedAt. restic restore's self-reported seconds (a hostile boundary, Rule
	// 5) cover only the restore, not our normalization + rename, so the injected
	// clock is the authoritative duration for both file and directory extracts.
	result.Elapsed = a.Clock.Now().Sub(startedAt)

	a.logExtractSuccess(req, result)
	return result, nil
}

// dispatchExtract routes every mode through the single restore driver. Both file
// and directory extracts are `restic restore` now; the file/directory difference
// is carried entirely in the resticx params (Source rebase + IncludePath), built
// by extractTreeParams.
func (a *App) dispatchExtract(ctx context.Context, r config.Repo, material secrets.Material, req ExtractRequest, staging string, result *ExtractResult, onProgress func(ExtractProgress)) error {
	return a.extractTree(ctx, r, material, req, staging, result, onProgress)
}

// extractTreeParams builds the resticx restore params from the request and
// staging dir, setting the RAW Source / IncludePath (resticx escapes the include
// to a literal pattern). The three shapes:
//
//   - Directory       → Source: req.Source,           IncludePath: ""
//   - File, flattened → Source: path.Dir(req.Source), IncludePath: "/"+base  (default)
//   - File, nested    → Source: "",                   IncludePath: req.Source
//
// A root-level file ("/foo") flattens via Source path.Dir = "/" (a bare-snapshot
// source) and IncludePath "/foo", so flattened and nested coincide — correct.
func extractTreeParams(req ExtractRequest, staging string) resticx.ExtractTreeParams {
	p := resticx.ExtractTreeParams{
		SnapshotID: req.SnapshotID,
		Source:     req.Source,
		Target:     staging,
		DryRun:     req.DryRun,
	}
	if req.Mode == ExtractFile {
		if req.Nested {
			p.Source = ""
			p.IncludePath = req.Source
		} else {
			p.Source = path.Dir(req.Source)
			p.IncludePath = "/" + path.Base(req.Source)
		}
	}
	return p
}

// extractTree drives resticx.ExtractTree and flattens its events into result /
// onProgress. The summary's file count is recorded here but, for a live run, is
// later overwritten by the metadata normalizer's authoritative split.
func (a *App) extractTree(ctx context.Context, r config.Repo, material secrets.Material, req ExtractRequest, staging string, result *ExtractResult, onProgress func(ExtractProgress)) error {
	params := extractTreeParams(req, staging)
	onEvent := func(ev resticx.ExtractTreeEvent) error {
		switch ev.Kind {
		case resticx.ExtractTreeStatus:
			if onProgress != nil {
				onProgress(ExtractProgress{
					BytesDone:      ev.BytesRestored,
					BytesTotal:     ev.TotalBytes,
					FilesDone:      int(ev.FilesRestored),
					FilesTotal:     int(ev.TotalFiles),
					SecondsElapsed: float64(ev.SecondsElapsed),
				})
			}
		case resticx.ExtractTreeVerboseStatus:
			if req.DryRun {
				result.DryRunPreview = append(result.DryRunPreview, ExtractPreviewItem{
					Action: ev.Action, Item: ev.Item, Size: ev.Size,
				})
			}
		case resticx.ExtractTreeSummary:
			result.Files = int(ev.FilesRestored)
			result.Bytes = ev.BytesRestored
		}
		return nil
	}
	// result.Elapsed is set by Extract from the injected clock (wall-clock for the
	// whole op); restic's self-reported seconds feed only the live progress line.
	return a.Restic.ExtractTree(ctx, targetOf(r), resticCreds(material), params, onEvent)
}

// freshTargetCheck refuses an extract whose staging or final dir already exists.
// Both checks run for dry-runs and real runs so a conflict surfaces before the
// user commits. The rule is path occupancy, so it Lstats (never Stat): a dangling
// symlink at either path is an occupant and must fail here, not later as a rename
// error. The sentinels are path-free; a non-ENOENT failure is wrapped path-free.
func freshTargetCheck(staging, final string) error {
	if _, err := os.Lstat(staging); err == nil {
		return ErrExtractStagingExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return pathFreeExtractErr("stat staging", err)
	}
	if _, err := os.Lstat(final); err == nil {
		return ErrExtractFinalExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return pathFreeExtractErr("stat target", err)
	}
	return nil
}

// logExtractSuccess emits the only success log line. It carries operation kind,
// repo name, snapshot short ID, a phase marker, and counts — never a path.
func (a *App) logExtractSuccess(req ExtractRequest, result ExtractResult) {
	kind := "extract.tree"
	if req.Mode == ExtractFile {
		kind = "extract.file"
	}
	a.logger().Info("extract finished",
		"op", kind,
		"repo", req.Repo,
		"snapshot", req.SnapshotShort,
		"phase", "finish",
		"files", result.Files,
		"dirs", result.Dirs,
		"bytes", result.Bytes,
		"unsafe_symlinks", result.UnsafeSymlinks,
		"other", result.Other,
	)
}

// pathFreeCause strips the filesystem path from an *os.PathError / *os.LinkError
// so a local IO failure can be surfaced without leaking the source/target path.
// A cause carrying no path is returned verbatim.
func pathFreeCause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// pathFreeExtractErr wraps a local filesystem failure with a fixed label and its
// path stripped.
func pathFreeExtractErr(label string, err error) error {
	return fmt.Errorf("extract: %s: %w", label, pathFreeCause(err))
}
