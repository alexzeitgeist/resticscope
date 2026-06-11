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
	unsafeSymlinkKeep        = unsafeSymlinkPolicy(config.UnsafeSymlinksKeep)        // leave verbatim + warn (restic-faithful)
	unsafeSymlinkSkip        = unsafeSymlinkPolicy(config.UnsafeSymlinksSkip)        // remove from the output
	unsafeSymlinkPlaceholder = unsafeSymlinkPolicy(config.UnsafeSymlinksPlaceholder) // replace with an inert text file recording the target
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

	// DiffContainer, when non-empty, marks this as one side of a diff extract
	// and names the pair container the side publishes under: final becomes
	// <target_root>/<repo>/<DiffContainer>/<short>/<mirror>, so the two sides of
	// a pair land as sibling snapshot-id roots inside one container. The shape
	// is asserted as diff-<short>-<short> with distinct shorts, one of which is
	// SnapshotShort — a container can never be named for a pair this snapshot
	// is not part of. The canonical <repo>/<short>/ mirror trees stay reserved
	// for full extracts; a sparse changed-paths tree never shadows them.
	DiffContainer string

	// IncludePaths optionally narrows an ExtractDirectoryTree restore to exactly
	// these paths inside Source (each equal to Source or strictly under it) —
	// the diff extract's changed-paths-only restore. Empty means the whole
	// subtree. Every path must already be in model.CleanBrowsePath form; the
	// list is bounded by MaxDiffExtractIncludes / MaxDiffExtractIncludeBytes so
	// the assembled restic argv stays far below any platform ARG_MAX.
	IncludePaths []string

	// Privileged requests the restore run as root (via the sudo extract helper)
	// so restic applies the snapshot's file ownership, which it skips as
	// non-root. The pipeline is otherwise identical; App.Extract routes a
	// privileged request to the helper re-exec instead of an in-process restic.
	Privileged bool
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

// MaxDiffExtractIncludes / MaxDiffExtractIncludeBytes bound one request's
// include list. A multi-include restore delivers its patterns on an fd-4
// pattern file (resticx), so ARG_MAX no longer binds; these are runaway
// guards — restic matches every restored node against every pattern, so a
// six-figure list degrades the restore itself, and a selection that large is
// better served by a plain subtree extract. (Patterns a line-based file
// cannot carry — $/newline-bearing names — spill to argv under resticx's own
// separate byte budget.) The TUI pre-checks the same caps to surface a
// friendly "narrow the filter" hint before a request is even built;
// PlanExtractPaths re-asserts them at the boundary.
const (
	MaxDiffExtractIncludes     = 50_000
	MaxDiffExtractIncludeBytes = 8 << 20
)

// extractSnapshotShortRe is restic's short-ID shape: exactly 8 lowercase hex.
var extractSnapshotShortRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// extractDiffContainerRe is the diff-pair container directory shape: the two
// short ids in first → second display order.
var extractDiffContainerRe = regexp.MustCompile(`^diff-([0-9a-f]{8})-([0-9a-f]{8})$`)

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

	// A diff container must name a real pair this snapshot belongs to, so the
	// published side can never land in a container named for other snapshots.
	if req.DiffContainer != "" {
		mm := extractDiffContainerRe.FindStringSubmatch(req.DiffContainer)
		if mm == nil || mm[1] == mm[2] ||
			(mm[1] != req.SnapshotShort && mm[2] != req.SnapshotShort) {
			return "", "", invalidExtractRequest("diff_container")
		}
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

	// Every include must be a clean rooted path at or inside Source, and the
	// list must fit the argv budget. Asserted here — before any hash, staging
	// dir, or restic spawn — like the Source contract above, so a malformed or
	// oversized list can never leave staging behind or reach an exec boundary.
	if len(req.IncludePaths) > MaxDiffExtractIncludes {
		return "", "", invalidExtractRequest("include_paths")
	}
	if len(req.IncludePaths) > 0 {
		srcPrefix := req.Source
		if srcPrefix != "/" {
			srcPrefix += "/"
		}
		total := 0
		for _, p := range req.IncludePaths {
			total += len(p)
			if p == "" || p == "/" ||
				strings.ContainsRune(p, '\x00') ||
				p != model.CleanBrowsePath(p) ||
				(p != req.Source && !strings.HasPrefix(p, srcPrefix)) {
				return "", "", invalidExtractRequest("include_paths")
			}
		}
		if total > MaxDiffExtractIncludeBytes {
			return "", "", invalidExtractRequest("include_paths")
		}
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
	// to the snapshot dir itself. A diff extract inserts its pair container above
	// the per-snapshot dir, so the two sides publish as sibling snapshot-id roots.
	relpath := filepath.FromSlash(strings.TrimPrefix(req.Source, "/")) // "" when Source=="/"
	repoDir := filepath.Join(targetRoot, repoSlug)
	snapDir := filepath.Join(repoDir, req.SnapshotShort)
	if req.DiffContainer != "" {
		snapDir = filepath.Join(repoDir, req.DiffContainer, req.SnapshotShort)
	}
	final = filepath.Join(snapDir, relpath) // == snapDir when relpath==""

	// The container joins the hash input so a kept-on-failure staging dir from a
	// plain extract of the same (snapshot, source) never blocks its diff twin —
	// the two runs are different operations and deserve distinct staging names.
	hashInput := req.Source
	if req.DiffContainer != "" {
		hashInput = req.DiffContainer + "\x00" + req.Source
	}
	sum := sha256.Sum256([]byte(hashInput))
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
		if len(req.IncludePaths) > 0 {
			// The file mode's whole contract is "exactly this one regular file";
			// a changed-paths list belongs to the directory-tree shape.
			return invalidExtractRequest("include_paths")
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

	// 4b. A privileged request runs the rest of the pipeline (steps 5–9) in a
	// root helper process (sudo + self re-exec): elevating only the restic
	// child is not enough, because the normalizer walk and the hardlink publish
	// both fail as non-root against a root-owned tree. Steps 1–4 above ran
	// in-process — fail fast, and credentials resolve with the user's own
	// environment (secrets_command never runs under sudo).
	if req.Privileged {
		return a.extractPrivileged(ctx, req, staging, r, material, onProgress)
	}

	result, err = runExtractPipeline(ctx, req, staging, final, extractPipeline{
		drv:        a.Restic,
		target:     targetOf(r),
		creds:      resticCreds(material),
		policy:     unsafeSymlinkPolicy(a.Cfg.Extract.UnsafeSymlinks),
		timeout:    a.Cfg.Extract.ExtractTimeout.Std(),
		clock:      a.Clock,
		mkdir:      func(dir string) error { return os.MkdirAll(dir, 0o700) },
		onProgress: onProgress,
	})
	if err != nil {
		return result, err
	}
	a.logExtractSuccess(req, result)
	return result, nil
}

// extractPipeline carries the per-caller seams of the shared restore pipeline.
// The in-process path and the root helper run the exact same steps and differ
// only at these injected points.
type extractPipeline struct {
	drv    extractTreeDriver
	target resticx.Target
	creds  resticx.Creds
	policy unsafeSymlinkPolicy
	clock  Clock

	// timeout bounds the restore; <=0 means no local deadline (the helper runs
	// without one when the parent sent no bound — its stdin-EOF kill switch
	// still ends the run).
	timeout time.Duration

	// noCache forces --no-cache on the restore. The root helper sets it as the
	// fallback when it cannot share the invoking user's repo cache (no cache
	// dir in the payload, unknown invoker, or cache-dir setup failure).
	noCache bool

	// mkdir creates every scaffolding dir AROUND the extracted content (the
	// staging parent and the mirror ancestors): plain 0700 MkdirAll in-process,
	// mkdirAllOwned in the helper so the invoking user keeps ownership of dirs
	// created next to root-owned content.
	mkdir func(dir string) error

	onProgress func(ExtractProgress)
}

// runExtractPipeline is the shared core of an extract — steps 5–9 of
// App.Extract: create staging, run the restore under the per-op timeout,
// normalize the staged tree's metadata, and publish staging → final. Both the
// in-process path and the root helper (runHelperExtract) call it, so the two
// can never drift; callers own validation, cred resolution, and logging.
func runExtractPipeline(ctx context.Context, req ExtractRequest, staging, final string, p extractPipeline) (ExtractResult, error) {
	var result ExtractResult

	// startedAt anchors result.Elapsed against the injected clock. Mtimes are no
	// longer reset, so this is its only use.
	startedAt := p.clock.Now()

	// 5. Create the parent and staging dir. A pre-existing parent is left
	// untouched — the user owns its policy; only the new staging (and, via
	// rename, final) dirs are 0700.
	if err := p.mkdir(filepath.Dir(staging)); err != nil {
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
	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	// 7. Run the restore. Both file and directory extracts are `restic restore`
	// with the same params shape (rebase parent + --include the leaf, built by
	// extractTreeParams); the file/directory difference is only in
	// publishExtract's primitive (link vs rename).
	params := extractTreeParams(req, staging)
	params.NoCache = p.noCache
	if dispErr := driveExtractTree(runCtx, p.drv, p.target, p.creds, params, &result, p.onProgress); dispErr != nil {
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
	counts, nerr := normalizeExtractTreeMetadata(runCtx, staging, p.policy)
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
	if err := publishExtract(req.Mode, req.Source, staging, final, p.mkdir); err != nil {
		return result, err
	}
	result.FinalPath = final
	if req.Mode == ExtractFile {
		// FinalDir must be a directory the shell-here action can cd into; for a file
		// that is the containing mirror dir (a shared <short>/<dir>/ after a merge).
		result.FinalDir = filepath.Dir(final)
	} else if fi, statErr := os.Lstat(final); statErr == nil && !fi.IsDir() {
		// A diff extract of a single changed path publishes a non-dir leaf through
		// the tree mode; shell-here needs the containing dir all the same.
		result.FinalDir = filepath.Dir(final)
	} else {
		result.FinalDir = final
	}

	// Total wall-clock for the operation, measured against the injected clock from
	// startedAt. restic restore's self-reported seconds (a hostile boundary, Rule
	// 5) cover only the restore, not our normalization + rename, so the injected
	// clock is the authoritative duration for both file and directory extracts.
	result.Elapsed = p.clock.Now().Sub(startedAt)
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
// A directory-mode publish whose reconstructed node turns out to be a
// non-directory (a diff extract of one changed file, symlink, or special —
// diff entries attest no node type, so those route through tree mode) uses the
// link primitive, keeping its atomic no-replace guarantee for every leaf type;
// linkNoFollow links the node itself, never a symlink's target.
//
// After publishing, the now-empty staging container is removed (best-effort).
// Staging lives at repo level, so mkdir(filepath.Dir(final)) here is the only
// place the leaf's parents are created — doing it lazily means an extract
// interrupted during restic leaves only the repo-level staging dir, no empty
// mirror ancestors, and threading the caller's mkdir means the helper's
// ownership policy applies wherever ancestors are created, not just where the
// helper guessed they would be. EEXIST/EXDEV surface as ErrExtractFinalExists/
// ErrExtractRenameFailed; the returned errors stay path-free.
func publishExtract(mode ExtractMode, source, staging, final string, mkdir func(dir string) error) error {
	if err := mkdir(filepath.Dir(final)); err != nil {
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
		// A diff extract of a single changed path reconstructs a non-directory
		// node here (the tree mode is the diff shape regardless of leaf type, since
		// diff entries carry no node-type attestation). EVERY non-directory leaf —
		// regular file, symlink, fifo, device — publishes through the no-replace
		// link primitive: rename(2) would silently replace a non-directory
		// occupant created after an advisory Lstat, so stat-then-rename is
		// race-safe only for real directory nodes.
		if fi, lerr := os.Lstat(node); lerr == nil && !fi.IsDir() {
			if err := linkExtractNode(node, final); err != nil {
				return err
			}
			break
		}
		if _, err := os.Lstat(final); err == nil { // advisory early refuse
			return ErrExtractFinalExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return pathFreeExtractErr("stat target", err)
		}
		// rename cannot overwrite a non-empty dir; for a directory node, safe.
		if err := os.Rename(node, final); err != nil {
			return fmt.Errorf("%w: %v", ErrExtractRenameFailed, pathFreeCause(err))
		}
	case ExtractFile:
		if err := linkExtractNode(node, final); err != nil {
			return err
		}
	}
	// Remove the now-empty staging container. Skipped for the whole-snapshot case,
	// where staging was itself renamed into final above.
	if node != staging {
		_ = os.Remove(staging)
	}
	return nil
}

// linkExtractNode publishes a non-directory node: hard-link into place —
// linkat(2) without AT_SYMLINK_FOLLOW fails EEXIST and never replaces, closing
// the TOCTOU race a stat-then-rename cannot, and links a symlink node itself
// rather than its target on every supported platform — then unlink the staging
// copy (best-effort; final already holds the inode).
func linkExtractNode(node, final string) error {
	if err := linkNoFollow(node, final); err != nil {
		if errors.Is(err, os.ErrExist) { // lost the race / occupied → refuse, never clobber
			return ErrExtractFinalExists
		}
		return fmt.Errorf("%w: %v", ErrExtractRenameFailed, pathFreeCause(err))
	}
	_ = os.Remove(node)
	return nil
}

// extractTreeParams builds the resticx restore params from the request and
// staging dir, setting the RAW Source / IncludePaths (resticx escapes each
// include to a literal pattern). File and directory share ONE shape — rebase the
// parent, reconstruct the leaf at staging/<base> via --include:
//
//   - File / directory → Source: path.Dir(req.Source), includes: ["/"+base]
//   - Diff extract → same rebase, one include per changed path, each rebased
//     from req.Source onto "/"+base (req.Source itself maps to exactly "/"+base)
//   - Whole snapshot "/" → bare restore (Source "", request includes verbatim;
//     none for a full extract): staging itself
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
		// staging itself becomes the published tree. A changed-paths list (already
		// rooted at "/") selects within it verbatim.
		p.IncludePaths = append([]string(nil), req.IncludePaths...)
		return p
	}
	p.Source = path.Dir(req.Source)
	base := "/" + path.Base(req.Source)
	if len(req.IncludePaths) == 0 {
		p.IncludePaths = []string{base}
		return p
	}
	incs := make([]string, len(req.IncludePaths))
	for i, inc := range req.IncludePaths {
		// PlanExtractPaths asserted inc == Source or Source+"/..."; rebasing onto
		// the reconstructed leaf keeps every include inside staging/<base>.
		incs[i] = base + strings.TrimPrefix(inc, req.Source)
	}
	p.IncludePaths = incs
	return p
}

// ExtractDiffTargetDir returns the diff-pair container directory a diff-extract
// request publishes its snapshot root under — the path the TUI's review and
// done screens show instead of one side's leaf (and the shell-here landing
// dir, where both roots are visible). Derivation only; it touches no
// filesystem and asserts just the fields it consumes (PlanExtractPaths owns
// full validation).
func ExtractDiffTargetDir(cfg config.Extract, req ExtractRequest) (string, error) {
	if req.DiffContainer == "" {
		return "", invalidExtractRequest("diff_container")
	}
	repoSlug, err := SanitizeExtractSlug(req.Repo)
	if err != nil {
		return "", invalidExtractRequest("repo")
	}
	targetRoot := cfg.TargetRoot
	if req.TargetRoot != "" {
		targetRoot = req.TargetRoot
	}
	if targetRoot == "" || !filepath.IsAbs(targetRoot) {
		return "", invalidExtractRequest("target_root")
	}
	return filepath.Join(targetRoot, repoSlug, req.DiffContainer), nil
}

// extractTreeDriver is the one restic operation the restore pipeline needs.
// Declared consumer-side (rule 3) so the privileged helper can drive a bare
// resticx.Client through the same event flattening App.Extract uses, without
// constructing a full App.
type extractTreeDriver interface {
	ExtractTree(ctx context.Context, t resticx.Target, creds resticx.Creds, params resticx.ExtractTreeParams, onEvent func(resticx.ExtractTreeEvent) error) error
}

// driveExtractTree runs one restic restore via drv and flattens its events into
// result / onProgress. Shared verbatim by the in-process extract and the
// privileged helper so the two event paths can never drift. result.Elapsed is
// the caller's job (wall-clock from its injected clock); restic's self-reported
// seconds feed only the live progress line.
func driveExtractTree(ctx context.Context, drv extractTreeDriver, t resticx.Target, creds resticx.Creds, params resticx.ExtractTreeParams, result *ExtractResult, onProgress func(ExtractProgress)) error {
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
	return drv.ExtractTree(ctx, t, creds, params, onEvent)
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

// ExtractFreeSpace reports the free bytes on the filesystem that will hold an
// extraction planned at staging. Neither staging nor final exists at review
// time, so the probe walks up to the nearest existing ancestor; final is a
// sibling of staging under the same root, so one filesystem answers for both
// (the post-extract rename never crosses filesystems). known is false when no
// ancestor can be statted or the platform offers no statfs binding. Exported
// for the TUI's review-screen space preflight, and advisory only — a run that
// outgrows the filesystem still fails with restic's own error.
func ExtractFreeSpace(staging string) (free int64, known bool) {
	dir := staging
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, false
		}
		dir = parent
	}
	return freeBytesAt(dir)
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
		"privileged", req.Privileged,
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
