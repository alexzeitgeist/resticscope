package resticx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"

	json "github.com/goccy/go-json"
)

// ExtractTree drives subtree and single-file restores through one streaming
// boundary. Includes are escaped because restic treats them as globs; multi-file
// patterns safe for file parsing use fd 4 to avoid argv exposure, while the rest
// stay on argv under a byte budget. The restore JSON schema follows restic
// 0.18.1's internal/ui/restore and cmd/restic implementations.

// extractTreeStreamBuffer accommodates restore progress lines without the browse
// stream's larger allocation.
const extractTreeStreamBuffer = 1 << 16

// extractPathMask replaces scrubbed paths without triggering the residual-path
// guard itself.
const extractPathMask = "[path]"

// snapshotIDHexLen requires a full lowercase SHA-256 ID so restic never performs
// snapshot-prefix resolution for an extraction request.
const snapshotIDHexLen = 64

// extractPatternFilePath references the pattern pipe on Linux and Darwin. fd 3
// is always reserved first, so this descriptor cannot shift.
const extractPatternFilePath = "/dev/fd/4"

// maxArgvIncludeBytes budgets each argv-routed include's flag, pattern, and NUL
// terminators. The 512 KiB cap reserves half of Darwin's 1 MiB ARG_MAX for base
// arguments and the environment, reducing E2BIG risk after staging begins; those
// inputs are not bounded here, so the cap is not a complete exec-size guarantee.
const maxArgvIncludeBytes = 512 << 10

// argvIncludeFlagOverhead accounts for the flag and both NUL terminators.
const argvIncludeFlagOverhead = len("--include") + 2

// Extract validation sentinels never echo rejected paths and are safe to log.
var (
	// ErrExtractInvalidSnapshotID rejects references other than full lowercase-hex
	// IDs, preventing restic from choosing an unconfirmed snapshot.
	ErrExtractInvalidSnapshotID = errors.New("resticx: snapshot ID is not a concrete hex ID")
	// ErrExtractInvalidSource rejects sources not already normalized by
	// model.CleanBrowsePath; this layer never repairs them.
	ErrExtractInvalidSource = errors.New("resticx: extract source is not a cleaned absolute path")
	// ErrExtractInvalidTarget rejects an empty or non-absolute --target.
	ErrExtractInvalidTarget = errors.New("resticx: extract target is not an absolute path")
	// ErrExtractInvalidInclude rejects paths that are unclean, unrooted, or root;
	// root would violate the changed-paths-only selection.
	ErrExtractInvalidInclude = errors.New("resticx: extract include path is not a cleaned non-root absolute path")
	// ErrExtractArgvIncludeOverflow rejects argv-routed patterns exceeding
	// maxArgvIncludeBytes.
	ErrExtractArgvIncludeOverflow = errors.New("resticx: too many include paths require command-line delivery; the argv would exceed the platform limit")
	// ErrExtractPatternsUnsupported indicates that the configured stream runner
	// cannot carry the fd-4 pattern file.
	ErrExtractPatternsUnsupported = errors.New("resticx: stream runner cannot deliver the fd-4 include pattern file")
)

// ExtractTreeParams describes one restore. The caller creates the staging Target
// with mode 0700 before invoking ExtractTree.
type ExtractTreeParams struct {
	// SnapshotID is the concrete lowercase-hex snapshot ID. Never "latest".
	SnapshotID string

	// Source is a cleaned, rooted subtree; "" or "/" selects the whole snapshot.
	// It must equal model.CleanBrowsePath(Source) and is joined to SnapshotID as
	// restic's <snap>:<src>.
	Source string

	// Target is the absolute staging dir handed to restic --target.
	Target string

	// IncludePaths contains cleaned, rooted, non-root literal snapshot paths;
	// empty selects the whole subtree. Paths are relative to Source for matching
	// and escaped before use with restic's glob-based --include.
	IncludePaths []string

	// NoCache adds --no-cache, preventing privileged restores from creating a root
	// cache or root-owned files in the user's cache.
	NoCache bool
}

// ExtractTreeEventKind tags which ExtractTreeEvent fields are populated.
type ExtractTreeEventKind int

const (
	// ExtractTreeStatus is a live-progress tick (message_type "status").
	ExtractTreeStatus ExtractTreeEventKind = iota
	// ExtractTreeSummary is the final tally (message_type "summary").
	ExtractTreeSummary
	// ExtractTreeError is a per-item error. Restic 0.18 sends these to stderr, but
	// the kind preserves compatibility with future stdout schemas.
	ExtractTreeError
)

// ExtractTreeEvent represents one restic restore JSON message. Kind selects the
// meaningful fields; counters follow the restic 0.18.1 restore schema and are
// represented as int64 instead of restic's uint64.
type ExtractTreeEvent struct {
	Kind ExtractTreeEventKind

	// Progress counters - populated for Status and Summary.
	PercentDone    float64 // Status only; Summary omits percent_done
	SecondsElapsed int64
	TotalFiles     int64
	FilesRestored  int64
	FilesSkipped   int64
	FilesDeleted   int64
	TotalBytes     int64
	BytesRestored  int64
	BytesSkipped   int64

	// Error-event strings may contain paths. This layer never copies them into a
	// returned Error; callers keep them in memory only.
	Item         string
	ErrorMessage string
	During       string
}

// restoreMessage decodes the union selected by message_type.
type restoreMessage struct {
	MessageType    string  `json:"message_type"`
	PercentDone    float64 `json:"percent_done"`
	SecondsElapsed int64   `json:"seconds_elapsed"`
	TotalFiles     int64   `json:"total_files"`
	FilesRestored  int64   `json:"files_restored"`
	FilesSkipped   int64   `json:"files_skipped"`
	FilesDeleted   int64   `json:"files_deleted"`
	TotalBytes     int64   `json:"total_bytes"`
	BytesRestored  int64   `json:"bytes_restored"`
	BytesSkipped   int64   `json:"bytes_skipped"`
	Item           string  `json:"item"`
	Error          struct {
		Message string `json:"message"`
	} `json:"error"`
	During string `json:"during"`
}

// toEvent rejects unknown message types for forward-compatible skipping.
func (m restoreMessage) toEvent() (ExtractTreeEvent, bool) {
	switch m.MessageType {
	case "status":
		return ExtractTreeEvent{
			Kind: ExtractTreeStatus, PercentDone: m.PercentDone, SecondsElapsed: m.SecondsElapsed,
			TotalFiles: m.TotalFiles, FilesRestored: m.FilesRestored, FilesSkipped: m.FilesSkipped,
			FilesDeleted: m.FilesDeleted, TotalBytes: m.TotalBytes, BytesRestored: m.BytesRestored,
			BytesSkipped: m.BytesSkipped,
		}, true
	case "summary":
		return ExtractTreeEvent{
			Kind: ExtractTreeSummary, SecondsElapsed: m.SecondsElapsed, TotalFiles: m.TotalFiles,
			FilesRestored: m.FilesRestored, FilesSkipped: m.FilesSkipped, FilesDeleted: m.FilesDeleted,
			TotalBytes: m.TotalBytes, BytesRestored: m.BytesRestored, BytesSkipped: m.BytesSkipped,
		}, true
	case "error":
		return ExtractTreeEvent{Kind: ExtractTreeError, ErrorMessage: m.Error.Message, During: m.During, Item: m.Item}, true
	default:
		return ExtractTreeEvent{}, false
	}
}

// ExtractTree restores a snapshot subtree and passes JSON progress to onEvent.
// IncludePaths limits restoration to matching nodes and their parent directories;
// callback errors cancel the child and are returned unchanged. ExtractTree adds
// no timeout because the caller owns the operation deadline.
func (c *Client) ExtractTree(ctx context.Context, t Target, creds Creds, params ExtractTreeParams, onEvent func(ExtractTreeEvent) error) error {
	args, patterns, err := buildExtractTreeArgs(params)
	if err != nil {
		return err // path-free validation sentinel; no argv was produced
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	full := prependBackendOpts(t, args...)

	env := c.buildEnv(t, creds)
	st := &extractTreeStream{onEvent: onEvent, cancel: cancel}
	runner, err := c.streamRunner()
	if err != nil {
		return err
	}
	var stderr []byte
	var runErr error
	if len(patterns) > 0 {
		// Multi-include patterns travel on fd 4 rather than in argv.
		pat, ok := runner.(PatternStreamRunner)
		if !ok {
			return ErrExtractPatternsUnsupported
		}
		stderr, runErr = pat.RunStreamPatterns(runCtx, env, creds.ResticPassword, patterns, st.consume, full...)
	} else {
		stderr, runErr = runner.RunStream(runCtx, env, creds.ResticPassword, st.consume, full...)
	}

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// Caller cancellation takes precedence over secondary failures.
		return context.Canceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return c.classify(ctx, "restore", runErr, c.sanitizeExtractStderr(stderr, params))
	case st.cbErr != nil:
		// Keep callback failures distinct from restic failures.
		return st.cbErr
	case st.decodeErr != nil:
		return &Error{Kind: KindParse, Op: "restore", wrapped: st.decodeErr}
	case runErr == nil:
		if st.sawError {
			// A future stdout error record makes an otherwise clean exit partial.
			return &Error{Kind: KindPartial, Op: "restore"}
		}
		return nil
	case st.sawSummary:
		// A summary followed by nonzero exit identifies a partial restore without
		// exposing item errors from stderr.
		return &Error{Kind: KindPartial, Op: "restore", Code: exitCodeOf(runErr)}
	default:
		return c.classify(ctx, "restore", runErr, c.sanitizeExtractStderr(stderr, params))
	}
}

// buildExtractTreeArgs validates restore safety invariants and assembles ordered
// arguments without --path, --delete, or "latest". Includes are literal-escaped;
// a single include uses argv, while multi-include runs put file-safe patterns on
// fd 4 and budget the remaining argv patterns.
func buildExtractTreeArgs(p ExtractTreeParams) (args []string, patterns []byte, err error) {
	if err := assertCleanSnapshotID(p.SnapshotID); err != nil {
		return nil, nil, err
	}
	if err := assertCleanSource(p.Source); err != nil {
		return nil, nil, err
	}
	for _, inc := range p.IncludePaths {
		if inc == "" { // Empty is valid only as the absence of an include list.
			return nil, nil, ErrExtractInvalidInclude
		}
		if err := assertCleanIncludePath(inc); err != nil {
			return nil, nil, err
		}
	}
	if p.Target == "" || !filepath.IsAbs(p.Target) {
		return nil, nil, ErrExtractInvalidTarget
	}

	snapArg := p.SnapshotID
	if p.Source != "" && p.Source != "/" {
		// One argv element keeps source metacharacters inert.
		snapArg = p.SnapshotID + ":" + p.Source
	}

	args = []string{"--no-lock"}
	if p.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args,
		"restore",
		snapArg,
		"--target", p.Target,
		"--overwrite", "never",
		"--json",
	)
	if len(p.IncludePaths) == 1 {
		return append(args, "--include", literalIncludePattern(p.IncludePaths[0])), nil, nil
	}
	var file strings.Builder
	argvBytes := 0
	for _, inc := range p.IncludePaths {
		pat := literalIncludePattern(inc)
		if fileSafePattern(inc) {
			file.WriteString(pat)
			file.WriteByte('\n')
			continue
		}
		argvBytes += len(pat) + argvIncludeFlagOverhead
		if argvBytes > maxArgvIncludeBytes {
			return nil, nil, ErrExtractArgvIncludeOverflow
		}
		args = append(args, "--include", pat)
	}
	if file.Len() > 0 {
		args = append(args, "--include-file", extractPatternFilePath)
		patterns = []byte(file.String())
	}
	return args, patterns, nil
}

// fileSafePattern reports whether an include can use the fd-4 pattern file.
// Restic expands environment variables, splits lines, and trims whitespace in
// pattern files, so paths affected by those transformations remain on argv.
func fileSafePattern(p string) bool {
	return !strings.ContainsAny(p, "$\n\r") && p == strings.TrimSpace(p)
}

// literalIncludePattern escapes restic filepath.Match metacharacters while
// retaining path separators. Backslash escaping is unsupported on Windows, so
// the app permits this path only on Linux and Darwin.
func literalIncludePattern(p string) string {
	var b strings.Builder
	b.Grow(len(p) + 4)
	for _, r := range p {
		switch r {
		case '\\', '[', ']', '*', '?':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// assertCleanIncludePath accepts empty or a NUL-free, rooted, normalized,
// non-root path.
func assertCleanIncludePath(p string) error {
	if p == "" {
		return nil
	}
	if p == "/" ||
		strings.ContainsRune(p, '\x00') ||
		!strings.HasPrefix(p, "/") ||
		p != model.CleanBrowsePath(p) {
		return ErrExtractInvalidInclude
	}
	return nil
}

// assertCleanSnapshotID accepts only a full lowercase-hex ID, excluding latest,
// short prefixes, and other references that restic could resolve itself.
func assertCleanSnapshotID(id string) error {
	if len(id) != snapshotIDHexLen {
		return ErrExtractInvalidSnapshotID
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrExtractInvalidSnapshotID
		}
	}
	return nil
}

// assertCleanSource accepts empty, root, or a NUL-free path already normalized by
// model.CleanBrowsePath. It rejects rather than repairs other input.
func assertCleanSource(src string) error {
	if src == "" || src == "/" {
		return nil
	}
	if strings.ContainsRune(src, '\x00') {
		return ErrExtractInvalidSource
	}
	if src != model.CleanBrowsePath(src) {
		return ErrExtractInvalidSource
	}
	return nil
}

// sanitizeExtractStderr removes source, target, include, and basename fragments
// before stderr can enter an Error. Includes cover leaf names absent from the
// source mapping.
func (c *Client) sanitizeExtractStderr(stderr []byte, p ExtractTreeParams) []byte {
	return c.scrubExtractStderr(stderr, extractPathFragments(p.Source, p.Target, p.IncludePaths))
}

// scrubExtractStderr redacts secrets and known paths. Any remaining slash may
// identify an unknown path or repository, so it drops the entire stderr payload;
// only KindUnknown would otherwise render that payload.
func (c *Client) scrubExtractStderr(stderr []byte, frags []string) []byte {
	s := string(stderr)
	if c.Redact != nil {
		s = c.Redact(s)
	}
	for _, frag := range frags {
		if frag != "" {
			s = strings.ReplaceAll(s, frag, extractPathMask)
		}
	}
	if strings.ContainsRune(s, '/') {
		return nil
	}
	return []byte(s)
}

// extractPathFragments orders full paths before basenames so shorter fragments
// mask only remaining text.
func extractPathFragments(source, target string, includePaths []string) []string {
	frags := make([]string, 0, 4+2*len(includePaths))
	if target != "" {
		frags = append(frags, target, filepath.Base(target))
	}
	if source != "" && source != "/" {
		frags = append(frags, source, filepath.Base(source))
	}
	for _, inc := range includePaths {
		if inc != "" && inc != "/" {
			frags = append(frags, inc, filepath.Base(inc))
		}
	}
	return frags
}

// exitCodeOf returns a restic exit code, or zero for other errors.
func exitCodeOf(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 0
}

// extractTreeStream forwards restore events while retaining only terminal errors
// and partial-restore signals.
type extractTreeStream struct {
	onEvent func(ExtractTreeEvent) error
	cancel  context.CancelFunc

	cbErr      error // onEvent's error, surfaced verbatim
	decodeErr  error // a genuine JSON decode failure (not a clean EOF)
	sawSummary bool
	sawError   bool
}

// consume forwards known NDJSON messages. Callback errors cancel restic, while
// decode errors are retained for ExtractTree classification.
func (s *extractTreeStream) consume(r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, extractTreeStreamBuffer))
	for {
		var m restoreMessage
		err := dec.Decode(&m)
		if err == nil {
			ev, ok := m.toEvent()
			if !ok {
				continue // unknown message_type
			}
			switch ev.Kind {
			case ExtractTreeSummary:
				s.sawSummary = true
			case ExtractTreeError:
				s.sawError = true
			}
			if s.onEvent != nil {
				if cbErr := s.onEvent(ev); cbErr != nil {
					s.cbErr = cbErr
					s.cancel()
					return cbErr
				}
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		s.decodeErr = err
		return err
	}
}
