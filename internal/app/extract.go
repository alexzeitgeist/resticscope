package app

// Extraction validates explicit requests, restores into staging, normalizes
// metadata, and publishes only on success. This layer owns filesystem safety;
// resticx owns argv safety. Production filesystem and restic errors, and success
// logs, omit source and target paths. Extraction writes no cache state.

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

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

// ExtractMode selects the source-driven restic operation.
type ExtractMode int

const (
	// ExtractFile restores one regular file with its snapshot metadata.
	ExtractFile ExtractMode = iota
	// ExtractDirectoryTree restores a subtree or full snapshot.
	ExtractDirectoryTree
)

// unsafeSymlinkPolicy controls absolute or tree-escaping symlinks in staging.
// It holds the validated unsafe_symlinks setting; zero and unknown values keep
// the link as the fail-safe behavior. Defined here, not in extract_metadata.go,
// so the linux/darwin normalizer and the other-platform stub share one definition.
type unsafeSymlinkPolicy string

const (
	unsafeSymlinkKeep        = unsafeSymlinkPolicy(config.UnsafeSymlinksKeep)        // leave verbatim and warn
	unsafeSymlinkSkip        = unsafeSymlinkPolicy(config.UnsafeSymlinksSkip)        // remove from output
	unsafeSymlinkPlaceholder = unsafeSymlinkPolicy(config.UnsafeSymlinksPlaceholder) // replace with inert text recording the target
)

// extractMetaCounts contains path-free metadata-normalization totals that are
// safe to log across platform implementations.
type extractMetaCounts struct {
	Files          int // regular files (metadata preserved as restic restored it)
	Dirs           int // directories (excludes the staging root)
	UnsafeSymlinks int // symlinks whose target is absolute or escapes the tree
	Other          int // device/fifo/socket/other special nodes, left in place
}

// ExtractRequest fully describes one extraction. App.Extract validates its
// slugs, paths, and source type before touching the filesystem, independently of
// the caller that constructed it.
type ExtractRequest struct {
	// Repo is the logical name; only its slugged form reaches the filesystem.
	Repo string

	// SnapshotID is the verified concrete hex snapshot ID. Never "latest".
	SnapshotID string

	// SnapshotShort is the caller-supplied first eight hex characters of SnapshotID.
	SnapshotShort string

	// Source is a cleaned, rooted snapshot path. "/" selects the full snapshot.
	Source string

	// SourceName is the sanitized basename used in staging. It must match Source,
	// or SnapshotShort when Source is "/".
	SourceName string

	Mode ExtractMode

	// WasRegularFile must be true only for ExtractFile. Other source types are
	// rejected rather than treated as files or directory trees.
	WasRegularFile bool

	// TargetRoot optionally overrides cfg.Extract.TargetRoot for this one call.
	// Empty means use the configured root; non-empty must be absolute.
	TargetRoot string

	// DiffContainer optionally names the diff pair under which this side is
	// published. It must contain two distinct snapshot shorts, including
	// SnapshotShort, and keeps canonical snapshot trees separate.
	DiffContainer string

	// IncludePaths optionally limits a directory restore to clean paths at or below
	// Source. Empty restores the subtree; request count and bytes are bounded.
	IncludePaths []string

	// Privileged runs the same pipeline through the root helper so restic can
	// restore snapshot ownership.
	Privileged bool
}

// ExtractProgress is flattened restic progress for the TUI. It has no ETA
// because restic restore reports no remaining duration.
type ExtractProgress struct {
	BytesDone      int64
	BytesTotal     int64 // 0 until restic reports it
	FilesDone      int
	FilesTotal     int
	SecondsElapsed float64
}

// ExtractResult describes published output or owned staging left after failure.
// Final paths are set only after publish; staging fields are set immediately
// after creation so callers can offer cleanup.
type ExtractResult struct {
	Files   int
	Dirs    int
	Bytes   int64
	Elapsed time.Duration

	// UnsafeSymlinks and Other count unsafe links and special nodes seen by the
	// normalizer. The result does not duplicate the config-only link policy.
	UnsafeSymlinks int
	Other          int

	// FinalDir is suitable for shell entry, while FinalPath is the published node.
	// They differ when a file or non-directory diff leaf is published.
	FinalDir  string
	FinalPath string

	StagingDir     string
	StagingCreated bool
}

// Extract errors identify fields or phases without including paths or values.
var (
	// ErrExtractInvalidRequest indicates a rejected field or source type.
	ErrExtractInvalidRequest = errors.New("extract: invalid request")
	// ErrExtractStagingExists indicates that the planned staging directory exists.
	ErrExtractStagingExists = errors.New("extract: staging directory already exists")
	// ErrExtractFinalExists indicates that the planned target is occupied.
	ErrExtractFinalExists = errors.New("extract: target already exists")
	// ErrExtractMetadataNormalization leaves staging unpublished after metadata
	// normalization fails.
	ErrExtractMetadataNormalization = errors.New("extract: staging metadata normalization failed")
	// ErrExtractRenameFailed indicates that publish failed, including across a
	// nested filesystem boundary. Staging remains available for cleanup.
	ErrExtractRenameFailed = errors.New("extract: staging rename failed")
)

// invalidExtractRequest adds a fixed, path-free field name to the sentinel.
func invalidExtractRequest(field string) error {
	return fmt.Errorf("%w: %s", ErrExtractInvalidRequest, field)
}

// MaxDiffExtractIncludes and MaxDiffExtractIncludeBytes bound pattern-matching
// cost rather than argv size. The TUI prechecks them and PlanExtractPaths
// enforces them at the boundary.
const (
	MaxDiffExtractIncludes     = 50_000
	MaxDiffExtractIncludeBytes = 8 << 20
)

// extractSnapshotShortRe is restic's short-ID shape: exactly 8 lowercase hex.
var extractSnapshotShortRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// extractDiffContainerRe matches two snapshot shorts in display order.
var extractDiffContainerRe = regexp.MustCompile(`^diff-([0-9a-f]{8})-([0-9a-f]{8})$`)

// extractSnapshotIDRe matches a full lowercase SHA-256 snapshot ID before any
// staging side effect.
var extractSnapshotIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SanitizeExtractSlug keeps ASCII letters, digits, '.', '_', and '-'; collapses
// other runs to '-'; trims leading '-' and '.'; and limits output to 64 bytes.
// It rejects empty output and is shared with callers that construct SourceName.
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

// PlanExtractPaths validates a request and deterministically derives staging and
// final paths without filesystem access. Invalid input returns a path-free
// ErrExtractInvalidRequest naming the field.
func PlanExtractPaths(cfg config.Extract, req ExtractRequest) (staging, final string, err error) { //nolint:gocyclo // a flat sequence of guard clauses re-asserting each request invariant before any work; splitting it would scatter the validation and obscure the ordering
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
	// Bind the target directory name to the snapshot actually extracted.
	if req.SnapshotShort != req.SnapshotID[:8] {
		return "", "", invalidExtractRequest("snapshot_short")
	}

	// Require the published side to belong to its named diff pair.
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
	// Reject malformed sources before hashing, staging, or invoking restic.
	if strings.ContainsRune(req.Source, '\x00') || req.Source != model.CleanBrowsePath(req.Source) {
		return "", "", invalidExtractRequest("source")
	}

	// Bound clean includes to Source before hashing, staging, or invoking restic.
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

	// A safe caller-supplied slug must still belong to the selected source.
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

	// Publish a mirror path under the snapshot, nested under its pair for diffs.
	relpath := filepath.FromSlash(strings.TrimPrefix(req.Source, "/")) // "" when Source=="/"
	repoDir := filepath.Join(targetRoot, repoSlug)
	snapDir := filepath.Join(repoDir, req.SnapshotShort)
	if req.DiffContainer != "" {
		snapDir = filepath.Join(repoDir, req.DiffContainer, req.SnapshotShort)
	}
	final = filepath.Join(snapDir, relpath) // == snapDir when relpath==""

	// Separate plain and diff staging names for the same snapshot source.
	hashInput := req.Source
	if req.DiffContainer != "" {
		hashInput = req.DiffContainer + "\x00" + req.Source
	}
	sum := sha256.Sum256([]byte(hashInput))
	hash := hex.EncodeToString(sum[:])[:16]
	// Keep staging outside mirrored content but beside the snapshot tree: real
	// content always lives under an 8-hex <short>/ dir, so a hidden repo-level
	// sibling can never collide with it. A 64-bit source hash reduces
	// same-basename collisions, and SourceName's 64-byte cap keeps the whole name
	// under NAME_MAX. Publish is atomic unless a nested mount separates the final
	// path.
	staging = filepath.Join(repoDir, ".resticscope-staging-"+req.SnapshotShort+"-"+req.SourceName+"-"+hash)
	return staging, final, nil
}

// checkExtractModeGate rejects unsupported source types and mode mismatches
// before filesystem or restic work.
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
			// File mode accepts exactly one regular file, not an include list.
			return invalidExtractRequest("include_paths")
		}
		return nil
	case ExtractDirectoryTree:
		if req.WasRegularFile {
			// Tree mode cannot restore a regular file as a whole subtree.
			return invalidExtractRequest("mode")
		}
		return nil
	default:
		return invalidExtractRequest("mode")
	}
}

// Extract validates and restores a request through staging, metadata
// normalization, and collision-checked publish. Production filesystem and
// restic errors omit source and target paths. onProgress is optional and runs
// from the resticx goroutine. A source rejected by restic leaves owned staging
// because this pipeline does not perform a preflight dry-run.
func (a *App) Extract(ctx context.Context, req ExtractRequest, onProgress func(ExtractProgress)) (ExtractResult, error) {
	var result ExtractResult

	staging, final, err := PlanExtractPaths(a.Cfg.Extract, req)
	if err != nil {
		return result, err
	}

	if err := checkExtractModeGate(req); err != nil {
		return result, err
	}

	// Refuse unsupported metadata and include handling before credentials,
	// staging, or restic execution.
	if !liveExtractSupported {
		return result, errors.New("extract: not supported on this platform")
	}

	r, ok := a.repo(req.Repo)
	if !ok {
		return result, fmt.Errorf("extract: %w", unknownRepoError(req.Repo))
	}
	// Credential is optional for local and SFTP backends; when set, it must resolve.
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return result, err
	}

	if err := FreshTargetCheck(staging, final); err != nil {
		return result, err
	}

	// Privileged extraction moves the remaining pipeline into the root helper;
	// validation and secret resolution stay in the user's environment.
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

// extractPipeline holds the seams that differ between in-process and helper runs.
type extractPipeline struct {
	drv    extractTreeDriver
	target resticx.Target
	creds  resticx.Creds
	policy unsafeSymlinkPolicy
	clock  Clock

	// timeout bounds restore; non-positive values rely on caller cancellation.
	timeout time.Duration

	// noCache avoids a cache the root helper cannot safely share with the user.
	noCache bool

	// mkdir creates scaffolding with ownership appropriate to the caller context.
	mkdir func(dir string) error

	onProgress func(ExtractProgress)
}

// runExtractPipeline creates staging, restores with a timeout, normalizes
// metadata, and publishes. Both in-process and helper paths share it; callers
// own validation, credentials, and logging.
func runExtractPipeline(ctx context.Context, req ExtractRequest, staging, final string, p extractPipeline) (ExtractResult, error) {
	var result ExtractResult

	startedAt := p.clock.Now()

	// Preserve existing parent policy; newly created staging is private.
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

	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	// File and directory restores share parameters; publish chooses link or rename.
	params := extractTreeParams(req, staging)
	params.NoCache = p.noCache
	if dispErr := driveExtractTree(runCtx, p.drv, p.target, p.creds, params, &result, p.onProgress); dispErr != nil {
		// Prefer local cancellation over resticx's interruption classification.
		if ce := runCtx.Err(); ce != nil {
			return result, fmt.Errorf("extract: %w", ce)
		}
		return result, fmt.Errorf("extract: %w", dispErr)
	}

	// Normalize both restore modes and derive live file and directory counts.
	counts, nerr := normalizeExtractTreeMetadata(runCtx, staging, p.policy)
	if nerr != nil {
		return result, nerr
	}
	// Restic's total_files includes directories, so the normalizer count wins.
	result.Files = counts.Files
	result.Dirs = counts.Dirs
	result.UnsafeSymlinks = counts.UnsafeSymlinks
	result.Other = counts.Other

	// Publish into the mirror only after successful restore and normalization.
	if err := publishExtract(req.Mode, req.Source, staging, final, p.mkdir); err != nil {
		return result, err
	}
	result.FinalPath = final
	if req.Mode == ExtractFile {
		// Shell entry for a file uses its containing mirror directory.
		result.FinalDir = filepath.Dir(final)
	} else if fi, statErr := os.Lstat(final); statErr == nil && !fi.IsDir() {
		// A non-directory diff leaf also uses its containing directory.
		result.FinalDir = filepath.Dir(final)
	} else {
		result.FinalDir = final
	}

	// Include normalization and publish in elapsed time, unlike restic's summary.
	result.Elapsed = p.clock.Now().Sub(startedAt)
	return result, nil
}

// publishExtract moves the restored node into its mirror path. It reuses
// ancestors without changing their modes and creates them only at publish.
// Non-directories use a no-follow hard link that atomically refuses occupancy.
// Directories use an advisory Lstat before rename: rename cannot replace a
// non-empty directory but can replace an empty directory created after the
// check. Whole snapshots publish staging itself. Errors remain path-free.
func publishExtract(mode ExtractMode, source, staging, final string, mkdir func(dir string) error) error {
	if err := mkdir(filepath.Dir(final)); err != nil {
		return pathFreeExtractErr("create target parent", err)
	}
	// Restic reconstructs a source at staging/base; a whole snapshot uses staging.
	node := staging
	if source != "/" {
		node = filepath.Join(staging, path.Base(source))
	}
	switch mode {
	case ExtractDirectoryTree:
		// Non-directory diff leaves use the no-replace link primitive.
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
		// A directory rename cannot overwrite a non-empty destination.
		if err := os.Rename(node, final); err != nil {
			return fmt.Errorf("%w: %w", ErrExtractRenameFailed, pathFreeCause(err))
		}
	case ExtractFile:
		if err := linkExtractNode(node, final); err != nil {
			return err
		}
	}
	// Whole-snapshot publish renames staging itself; other modes remove its shell.
	if node != staging {
		_ = os.Remove(staging)
	}
	return nil
}

// linkExtractNode publishes a node without following symlinks or replacing an
// existing target, then best-effort removes the staging link.
func linkExtractNode(node, final string) error {
	if err := linkNoFollow(node, final); err != nil {
		if errors.Is(err, os.ErrExist) { // Refuse an occupied target or lost race.
			return ErrExtractFinalExists
		}
		return fmt.Errorf("%w: %w", ErrExtractRenameFailed, pathFreeCause(err))
	}
	_ = os.Remove(node)
	return nil
}

// extractTreeParams rebases non-root sources to their parent and selects the leaf
// with literal includes; diff includes are rebased under the same leaf. Root
// restores target staging directly. This shared shape makes restic create the
// leaf and apply its snapshot metadata, which a bare source restore into a
// pre-created target root would omit.
func extractTreeParams(req ExtractRequest, staging string) resticx.ExtractTreeParams {
	p := resticx.ExtractTreeParams{SnapshotID: req.SnapshotID, Target: staging}
	if req.Source == "/" {
		// Root restores directly into staging and preserves any rooted include list.
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
		// Rebase validated includes beneath the reconstructed leaf.
		incs[i] = base + strings.TrimPrefix(inc, req.Source)
	}
	p.IncludePaths = incs
	return p
}

// ExtractDiffTargetDir derives the pair directory shown by diff review, done,
// and shell views. It performs no filesystem access and validates only the
// fields it consumes.
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

// extractTreeDriver lets in-process and helper restores share event handling.
type extractTreeDriver interface {
	ExtractTree(ctx context.Context, t resticx.Target, creds resticx.Creds, params resticx.ExtractTreeParams, onEvent func(resticx.ExtractTreeEvent) error) error
}

// driveExtractTree flattens restore events into result and optional progress.
// Callers calculate wall-clock elapsed time; restic elapsed seconds drive only
// live progress.
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

// FreshTargetCheck rejects occupied staging or final paths before side effects.
// It uses Lstat so dangling symlinks also count as occupied and returns path-free
// errors. The TUI uses the same check when overriding targets.
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

// ExtractFreeSpace reports advisory free bytes for the nearest existing staging
// ancestor. Staging and final normally share that filesystem. known is false
// when no ancestor or platform statfs support is available; extraction still
// relies on restic to report exhaustion.
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

// DeleteExtractStaging removes caller-confirmed staging and strips its path from
// any error.
func DeleteExtractStaging(staging string) error {
	if err := os.RemoveAll(staging); err != nil {
		return pathFreeExtractErr("delete staging", err)
	}
	return nil
}

// logExtractSuccess records operation metadata and counts without paths.
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

// pathFreeCause strips paths from filesystem errors and returns other causes
// unchanged.
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

// pathFreeExtractErr adds a fixed label to a path-free filesystem cause.
func pathFreeExtractErr(label string, err error) error {
	return fmt.Errorf("extract: %s: %w", label, pathFreeCause(err))
}
