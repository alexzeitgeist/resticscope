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
	// file lands at its true mirror path under the per-snapshot directory.
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

// extractMetaCounts tallies what the post-restore normalizer saw. It is
// count-only — it holds no paths — so it is safe to log. Like
// unsafeSymlinkPolicy above, it is defined here (not in extract_metadata.go) so
// the linux/darwin normalizer and the other-platform stub share one definition.
type extractMetaCounts struct {
	Files          int // regular files (metadata preserved as restic restored it)
	Dirs           int // directories (excludes the staging root)
	UnsafeSymlinks int // symlinks whose target is absolute or escapes the tree
	Other          int // device/fifo/socket/other special nodes, left in place
}

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

	// TargetRoot optionally overrides cfg.Extract.TargetRoot for this one call.
	// Empty means use the configured root; non-empty must be absolute.
	TargetRoot string
}

// ExtractProgress is the flattened progress the orchestrator hands to the TUI.
// resticx events never reach the TUI directly. There is no ETA field: restic
// restore's JSON schema carries no seconds_remaining.
type ExtractProgress struct {
	BytesDone      int64
	BytesTotal     int64 // 0 until restic reports it
	FilesDone      int
	FilesTotal     int
	SecondsElapsed float64
}

// ExtractResult is returned from App.Extract. FinalDir / FinalPath are set only
// after a clean publish. StagingDir/StagingCreated are set the instant the
// staging dir is created and are returned even alongside a later error, so the
// TUI can offer keep-or-delete for output this run owns.
type ExtractResult struct {
	Files   int
	Dirs    int
	Bytes   int64
	Elapsed time.Duration

	// UnsafeSymlinks counts symlinks whose target is absolute or escapes the
	// extracted tree; Other counts device/fifo/socket nodes left in place. Both
	// come from the normalizer walk (live tree extract only). The policy that
	// applied is the caller's own validated [extract] unsafe_symlinks config —
	// it is config-only, never per-request, so the result does not echo it.
	UnsafeSymlinks int
	Other          int

	// FinalDir is a directory the shell-here action can cd into; FinalPath is the
	// exact published node. For a directory extract the two coincide (the mirrored
	// tree root). For a file extract FinalDir is the file's containing mirror dir
	// (a shared <short>/<dir>/ on a merge) and FinalPath is the file itself.
	FinalDir  string
	FinalPath string

	StagingDir     string
	StagingCreated bool
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
	// ErrExtractFinalExists is returned when the planned final path/leaf is already
	// occupied (fresh-target check, the directory pre-publish re-check, or os.Link's
	// EEXIST for a file). The leaf may be a regular file, so the message says
	// "target", not "target directory".
	ErrExtractFinalExists = errors.New("extract: target already exists")
	// ErrExtractMetadataNormalization is returned when the post-restore staging
	// metadata pass fails or finds metadata that cannot be safely normalized.
	// Staging is left in place and no rename is attempted.
	ErrExtractMetadataNormalization = errors.New("extract: staging metadata normalization failed")
	// ErrExtractRenameFailed is returned when the publish move — os.Rename for a
	// directory, os.Link for a file — fails on the clean-completion path. With
	// repo-level staging and a deep mirror final, the two can straddle a filesystem
	// boundary when the user mounts a filesystem inside the repo dir, so EXDEV is
	// now possible here; staging is retained for keep/delete on failure.
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

	// Pure mirror tree: file and directory both land at their true path under a
	// per-snapshot directory. relpath is "" for the root source, so final collapses
	// to the snapshot dir itself.
	relpath := filepath.FromSlash(strings.TrimPrefix(req.Source, "/")) // "" when Source=="/"
	repoDir := filepath.Join(targetRoot, repoSlug)
	snapDir := filepath.Join(repoDir, req.SnapshotShort)
	final = filepath.Join(snapDir, relpath) // == snapDir when relpath==""

	sum := sha256.Sum256([]byte(req.Source))
	hash := hex.EncodeToString(sum[:])[:16]
	// Staging is a hidden dir at the REPO level (a sibling of the <short>/ snapshot
	// dirs), NOT inside the mirror subtree — so it can never collide with mirrored
	// snapshot content (real content always lives under an 8-hex <short>/ dir).
	// Same filesystem as final (all under repoDir), so rename/link stays atomic in
	// the normal app-created tree. <short>+SourceName+hash keep it unique per
	// (snapshot, source); the user never sees it. The hash is 16 hex chars (64
	// bits) — for same-basename sources it is the sole disambiguator, and 8 chars
	// (32 bits) would reach birthday-collision territory within one snapshot.
	// SourceName (<=64) bounds NAME_MAX (total stays well under 255).
	staging = filepath.Join(repoDir, ".resticscope-staging-"+req.SnapshotShort+"-"+req.SourceName+"-"+hash)
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
// normalizes the extracted-tree metadata, and renames staging → final on clean
// completion. Every returned error is path-free. onProgress is best-effort and
// nil-tolerant; it is called from the resticx goroutine.
//
// A bad source (one restic cannot restore) surfaces here only after the staging
// dir is created, since there is no pre-flight dry-run; the staging dir is then
// reported via StagingCreated so the caller can offer keep-or-delete.
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
	// path's --include escaping is not Windows-safe — so refuse the extract on an
	// unsupported platform BEFORE cred resolution / staging / restic, never
	// spawning restic with a non-literal include.
	if !liveExtractSupported {
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

	// 4. Fresh-target check.
	if err := FreshTargetCheck(staging, final); err != nil {
		return result, err
	}

	// startedAt anchors result.Elapsed against the injected clock. Mtimes are no
	// longer reset, so this is its only use.
	startedAt := a.Clock.Now()

	// 5. Create the parent and staging dir. A pre-existing parent is left
	// untouched — the user owns its policy; only the new staging (and, via
	// rename, final) dirs are 0700.
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

	// 6. Per-op timeout.
	runCtx, cancel := context.WithTimeout(ctx, a.Cfg.Extract.ExtractTimeout.Std())
	defer cancel()

	// 7. Run the restore. Both file and directory extracts are `restic restore`
	// with the same params shape (rebase parent + --include the leaf, built by
	// extractTreeParams); the file/directory difference is only in
	// publishExtract's primitive (link vs rename).
	if dispErr := a.extractTree(runCtx, r, material, req, staging, &result, onProgress); dispErr != nil {
		// Prefer the local timeout/cancel verdict so the app boundary surfaces a
		// context error regardless of how resticx classified the interruption.
		if ce := runCtx.Err(); ce != nil {
			return result, fmt.Errorf("extract: %w", ce)
		}
		return result, fmt.Errorf("extract: %w", dispErr)
	}

	// 8. Metadata normalization (every restore — file and directory — since both
	// route through restic restore into staging). A file is always Files=1, Dirs=0;
	// a directory walk adds its restored subdirectories to Dirs.
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

	// 9. Publish on clean completion: build the deep mirror ancestor chain, then
	// move staging into its true mirror path (rename for a directory, no-replace
	// link for a file). publishExtract owns the no-overwrite guarantee.
	if err := publishExtract(req.Mode, req.Source, staging, final); err != nil {
		return result, err
	}
	result.FinalPath = final
	if req.Mode == ExtractFile {
		// FinalDir must be a directory the shell-here action can cd into; for a file
		// that is the containing mirror dir (a shared <short>/<dir>/ after a merge).
		result.FinalDir = filepath.Dir(final)
	} else {
		result.FinalDir = final
	}

	// Total wall-clock for the operation, measured against the injected clock from
	// startedAt. restic restore's self-reported seconds (a hostile boundary, Rule
	// 5) cover only the restore, not our normalization + rename, so the injected
	// clock is the authoritative duration for both file and directory extracts.
	result.Elapsed = a.Clock.Now().Sub(startedAt)

	a.logExtractSuccess(req, result)
	return result, nil
}

// publishExtract moves the restored leaf node into its true mirror path. It is the
// authoritative no-overwrite primitive: the merge only ever fills empty space, so
// it builds the deep mirror ancestor chain (reusing pre-existing ancestors, never
// chmod'ing them) and then refuses any extract whose exact leaf is occupied.
//
// restic reconstructs the extracted node (file or directory) at staging/<base> via
// the rebase-parent + --include shape, so the PUBLISHED node is staging/<base> for
// both modes — and that is exactly why the leaf keeps its snapshot metadata
// (restic CREATES the node instead of restoring contents into resticscope's
// pre-made 0700 staging root). The whole-snapshot case (source=="/") has no <base>:
// staging itself is the tree. The two modes then need different primitives:
//
//   - Directory: os.Rename(node, final) is no-replace for data — rename(2) on a
//     directory source succeeds only onto a missing path or an empty directory
//     (ENOTDIR against a file, ENOTEMPTY against a non-empty dir), so it can never
//     overwrite data; the only residual is adopting an externally-created empty dir
//     (harmless). The Lstat is an advisory early refuse.
//   - File: os.Link + unlink. link(2) fails EEXIST if final exists and never
//     replaces, so it closes the TOCTOU race a publish-time Lstat cannot. There is
//     no Lstat here — os.Link is the no-replace guard. final is valid the instant
//     os.Link returns; the staging copy is then unlinked.
//
// After publishing, the now-empty staging container is removed (best-effort).
// Staging lives at repo level, so MkdirAll(filepath.Dir(final)) here is the only
// place the leaf's parents are created — doing it lazily means an extract
// interrupted during restic leaves only the repo-level staging dir, no empty
// mirror ancestors. EEXIST/EXDEV surface as ErrExtractFinalExists/
// ErrExtractRenameFailed; the returned errors stay path-free.
func publishExtract(mode ExtractMode, source, staging, final string) error {
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return pathFreeExtractErr("create target parent", err)
	}
	// The node restic reconstructed: staging/<base> for a real source, or staging
	// itself for the whole-snapshot ("/") case (which has no <base> to reconstruct).
	node := staging
	if source != "/" {
		node = filepath.Join(staging, path.Base(source))
	}
	switch mode {
	case ExtractDirectoryTree:
		if _, err := os.Lstat(final); err == nil { // advisory early refuse
			return ErrExtractFinalExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return pathFreeExtractErr("stat target", err)
		}
		// rename cannot overwrite a file or a non-empty dir; safe.
		if err := os.Rename(node, final); err != nil {
			return fmt.Errorf("%w: %v", ErrExtractRenameFailed, pathFreeCause(err))
		}
	case ExtractFile:
		// Hard-link the node into place (no-replace), then unlink the staging copy.
		if err := os.Link(node, final); err != nil {
			if errors.Is(err, os.ErrExist) { // lost the race / occupied → refuse, never clobber
				return ErrExtractFinalExists
			}
			return fmt.Errorf("%w: %v", ErrExtractRenameFailed, pathFreeCause(err))
		}
		_ = os.Remove(node) // best-effort; final already holds the inode
	}
	// Remove the now-empty staging container. Skipped for the whole-snapshot case,
	// where staging was itself renamed into final above.
	if node != staging {
		_ = os.Remove(staging)
	}
	return nil
}

// extractTreeParams builds the resticx restore params from the request and
// staging dir, setting the RAW Source / IncludePath (resticx escapes the include
// to a literal pattern). File and directory share ONE shape — rebase the parent,
// reconstruct the leaf at staging/<base> via --include:
//
//   - File / directory → Source: path.Dir(req.Source), IncludePath: "/"+base
//   - Whole snapshot "/" → bare restore (Source "", IncludePath ""): staging itself
//
// The shared shape is load-bearing for metadata fidelity: with --include, restic
// CREATES the leaf node (file or directory) and applies its snapshot
// mode/mtime/owner/xattrs. A bare `<snap>:<source>` restore would instead rebase
// the directory's CONTENTS into resticscope's pre-created 0700 staging root,
// leaving the leaf directory's own metadata unset — restic never touches its
// --target root's metadata. publishExtract then moves staging/<base> into the
// mirror path (link for a file, rename for a directory). A root-level source
// ("/foo") rebases via Source path.Dir = "/" (a bare-snapshot source).
func extractTreeParams(req ExtractRequest, staging string) resticx.ExtractTreeParams {
	p := resticx.ExtractTreeParams{SnapshotID: req.SnapshotID, Target: staging}
	if req.Source == "/" {
		// Whole-snapshot extract (future detail-view path): no parent to rebase and
		// no single node to reconstruct, so restic restores into staging directly and
		// staging itself becomes the published tree.
		return p
	}
	p.Source = path.Dir(req.Source)
	p.IncludePath = "/" + path.Base(req.Source)
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

// FreshTargetCheck refuses an extract whose staging or final dir already exists,
// so a conflict surfaces before any staging side effect. The rule is path
// occupancy, so it Lstats (never Stat): a dangling
// symlink at either path is an occupant and must fail here, not later as a rename
// error. The sentinels are path-free; a non-ENOENT failure is wrapped path-free.
// Exported so the TUI's target-override picker can re-plan and refuse with the
// same rule and sentinels App.Extract enforces.
func FreshTargetCheck(staging, final string) error {
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

// DeleteExtractStaging removes a staging directory after the user confirms the
// delete from the keep-or-delete prompt. It lives here so the staging lifecycle
// and the path-free error discipline stay in the layer that created the dir —
// a raw os.RemoveAll error embeds the staging path.
func DeleteExtractStaging(staging string) error {
	if err := os.RemoveAll(staging); err != nil {
		return pathFreeExtractErr("delete staging", err)
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
