package resticx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"resticscope/internal/model"

	json "github.com/goccy/go-json"
)

// extract_tree.go is the streaming restic boundary for `restic restore` — the
// single restore driver for BOTH a whole-subtree extract (`restic restore
// <snap>:<source> --target <dir>`) and a single-file extract (the same restore
// plus `--include <pattern>` selecting one node). It is the read-only sibling of
// browse/diff: it assembles a safe argv (every safety invariant in
// 00-framework.md §5 is encoded in buildExtractTreeArgs and asserted by argv
// tests), streams restic's --json progress to the caller, and classifies the
// exit code into the package's existing *Error / ErrorKind shape.
//
// CRUCIAL: restic's --include is a glob *pattern*, not a literal path — a path
// component carrying any of \ [ ] * ? is matched with filepath.Match, not by
// equality (verified in upstream/restic internal/filter/filter.go). So a real
// filename like a*.conf or backup[1].txt would over-match siblings or match the
// wrong node. The IncludePaths field on ExtractTreeParams therefore carries
// *literal* paths; literalIncludePattern backslash-escapes the metacharacter set
// so restic matches exactly those paths. The raw paths are what get validated
// and scrubbed from stderr; the escaped patterns never outlive the invocation.
//
// Delivery splits by shape. A single include (the canonical file / subtree
// extract) keeps its historical argv form. A multi-include run (the diff
// extract's changed-paths restore) delivers its patterns as a pattern file on
// fd 4 (`--include-file /dev/fd/4`, restic >= 0.17), so the argv can never
// outgrow ARG_MAX and the paths stay out of /proc/<pid>/cmdline — EXCEPT
// paths a line-based pattern file cannot carry faithfully: restic
// env-expands every line (verified live: a file named 'weird$NAME file' is
// unmatchable through a pattern file) and treats newlines / surrounding
// whitespace as structure, so those few stay on argv under a strict byte
// budget (fileSafePattern is the split rule).
//
// This file is the ONLY place that knows restic's restore --json schema; if
// restic 0.19 renames a field, only this file and its testdata fixtures change.
// The schema below was pinned against restic 0.18.1 (internal/ui/restore/json.go
// and cmd/restic/{cmd_restore,main}.go). Three points where reality diverges
// from the original design sketch, verified against that source:
//
//   - restic restore exits 0/1/10/11/12 only. There is no exit code 3 for
//     restore (exit 3 is backup's ErrInvalidSourceData). A partial restore —
//     some items failed — returns errors.Fatal("There were N errors") → exit 1.
//   - There is no error_count field in any restore message. Per-item errors are
//     emitted as message_type "error" to STDERR, not stdout, so the stdout
//     stream this parser reads never carries them under 0.18.1.
//   - restore prints its final "summary" (progress.Finish) BEFORE returning the
//     partial-error exit. So "exit 1 with a summary already seen" is the
//     reliable partial-vs-total signal, and that is what ExtractTree uses.
//
// The ExtractTreeError event kind is still parsed (an "error" line on stdout)
// for forward-compatibility with restic versions/configs that merge the streams.

// extractTreeStreamBuffer sizes the decoder's reader. Restore progress streams
// are small (a handful of lines), so the browse-sized 1 MiB buffer is overkill;
// 64 KiB comfortably holds any single restic JSON line.
const extractTreeStreamBuffer = 1 << 16

// extractPathMask replaces a source/target path fragment scrubbed out of restic
// stderr before it can enter a returned *Error. It carries no '/', so it never
// trips scrubExtractStderr's residual-path guard.
const extractPathMask = "[path]"

// snapshotIDHexLen is restic's full snapshot ID length: a SHA-256 digest in
// lowercase hex. 00-framework.md §5 requires the verified long ID verbatim — a
// short ID (an 8-char prefix) is display/target-name metadata only and must
// never reach restic, which would otherwise resolve it as an ambiguous prefix.
const snapshotIDHexLen = 64

// extractPatternFilePath is how the restore argv references the fd-4 pattern
// payload the runner wires up. /dev/fd/4 resolves to the pipe on both linux
// (procfs) and darwin (fd dup), and the number is fixed: runStreamFDs always
// fills the fd-3 password slot first, even for an empty password, so the
// reference can never shift.
const extractPatternFilePath = "/dev/fd/4"

// maxArgvIncludeBytes bounds the argv bytes the spillover includes may add in
// the multi-include shape. Only paths a pattern file cannot carry
// ($/newline/whitespace-edged names) land there, so hitting this means a
// pathological tree where hundreds of thousands of bytes of such names
// changed. The accounting is the full per-flag execve cost — the "--include"
// flag string, the escaped pattern, and one NUL terminator each — not just
// pattern bytes, so many SHORT spillover names cannot slip a half-megabyte of
// flag overhead past the budget. Refusing up front beats execve's E2BIG after
// staging side effects; 512 KiB of accounted spillover plus the base args and
// environment stays well under the tightest supported ARG_MAX (darwin: 1 MiB
// including the environment).
const maxArgvIncludeBytes = 512 << 10

// argvIncludeFlagOverhead is the per-spillover execve cost beyond the pattern
// itself: the "--include" flag string and the two NUL terminators of the pair.
const argvIncludeFlagOverhead = len("--include") + 2

// Extract path-validation sentinels. They are path-free by construction (they
// never echo the rejected value) so a rejection can be logged safely.
var (
	// ErrExtractInvalidSnapshotID rejects a snapshot reference that is not a
	// concrete lowercase-hex ID — in particular "latest", which would let restic
	// pick a snapshot the user never confirmed.
	ErrExtractInvalidSnapshotID = errors.New("resticx: snapshot ID is not a concrete hex ID")
	// ErrExtractInvalidSource rejects a source that is not already cleaned by
	// model.CleanBrowsePath. This layer asserts the cleaned-path contract rather
	// than silently repairing unclean input.
	ErrExtractInvalidSource = errors.New("resticx: extract source is not a cleaned absolute path")
	// ErrExtractInvalidTarget rejects an empty or non-absolute --target.
	ErrExtractInvalidTarget = errors.New("resticx: extract target is not an absolute path")
	// ErrExtractInvalidInclude rejects an include path that is not a cleaned,
	// rooted, non-root file path. Unlike Source, "/" is rejected: an include of
	// "/" is both too broad and contradicts the changed-paths-only invariant the
	// include filter exists to enforce.
	ErrExtractInvalidInclude = errors.New("resticx: extract include path is not a cleaned non-root absolute path")
	// ErrExtractArgvIncludeOverflow rejects a multi-include run whose
	// argv-routed patterns (the few a pattern file cannot carry) exceed
	// maxArgvIncludeBytes. Path-free like every validation sentinel.
	ErrExtractArgvIncludeOverflow = errors.New("resticx: too many include paths require command-line delivery; the argv would exceed the platform limit")
	// ErrExtractPatternsUnsupported is returned when a run carries an fd-4
	// pattern file but the wired stream runner is not a PatternStreamRunner.
	// Production wiring (ExecRunner) always supports it; this guards a test or
	// future wiring that forgot the capability.
	ErrExtractPatternsUnsupported = errors.New("resticx: stream runner cannot deliver the fd-4 include pattern file")
)

// ExtractTreeParams are the inputs to a single restic restore invocation. The
// caller (app.Extract) builds Source/Target/SnapshotID and creates the staging
// Target with 0700 before calling.
type ExtractTreeParams struct {
	// SnapshotID is the concrete lowercase-hex snapshot ID. Never "latest".
	SnapshotID string

	// Source is the cleaned, rooted subtree to extract ("/etc/nginx"). "" or "/"
	// means the whole snapshot (a future detail-view case). Anything else is
	// joined to SnapshotID with a literal ":" as restic's <snap>:<src> syntax.
	// Must already equal model.CleanBrowsePath(Source); this layer asserts that
	// and returns ErrExtractInvalidSource on violation — it does not re-clean.
	Source string

	// Target is the absolute staging dir handed to restic --target.
	Target string

	// IncludePaths are the RAW (cleaned, rooted) snapshot paths to select with
	// restic --include (one flag per path), or empty for "no include filter" (a
	// whole-subtree extract). Each is rebased-relative to Source (e.g.
	// "/vzdump.conf" under Source "/etc", or "/nginx/sub/x.conf" for a diff
	// extract under Source "/etc"). They are LITERAL paths, NOT restic patterns:
	// --include is a glob, so buildExtractTreeArgs escapes each via
	// literalIncludePattern at argv build. Each must be a cleaned non-root rooted
	// path (assertCleanIncludePath); this layer validates the raw values and
	// never validates the escaped patterns.
	IncludePaths []string

	// NoCache adds --no-cache, bypassing restic's local metadata cache entirely.
	// The privileged (sudo) extract helper sets it so a root-run restic neither
	// duplicates the user's multi-GB cache under /root nor poisons the user's
	// cache dir with root-owned files.
	NoCache bool
}

// ExtractTreeEventKind tags which ExtractTreeEvent fields are populated.
type ExtractTreeEventKind int

const (
	// ExtractTreeStatus is a live-progress tick (message_type "status").
	ExtractTreeStatus ExtractTreeEventKind = iota
	// ExtractTreeSummary is the final tally (message_type "summary").
	ExtractTreeSummary
	// ExtractTreeError is a restic per-item error (message_type "error"). restic
	// 0.18 routes these to stderr, so they are not seen on the stdout stream in
	// practice; the kind exists for forward-compat.
	ExtractTreeError
)

// ExtractTreeEvent is the parsed union of restic's restore --json messages. One
// is handed to onEvent per stdout line. Which fields are meaningful is decided
// by Kind. Counts are restic uint64 widened to int64 (the values fit) for
// ergonomic Go arithmetic. The field set tracks restic 0.18.1's actual schema;
// fields the sketch invented for a backup-style summary (files_new, dirs_*,
// data_blobs, error_count, seconds_remaining) do not exist for restore and are
// intentionally absent.
type ExtractTreeEvent struct {
	Kind ExtractTreeEventKind

	// Progress counters — populated for Status and Summary.
	PercentDone    float64 // Status only; Summary omits percent_done
	SecondsElapsed int64
	TotalFiles     int64
	FilesRestored  int64
	FilesSkipped   int64
	FilesDeleted   int64
	TotalBytes     int64
	BytesRestored  int64
	BytesSkipped   int64

	// restic error event — populated for Error. Item carries the error's item
	// path. These strings may carry paths; the caller decides what to do with
	// them (the in-memory-only discipline is enforced by the app/TUI). They are
	// NEVER written into a returned *Error by this layer.
	Item         string
	ErrorMessage string
	During       string
}

// restoreMessage decodes one restic restore --json line. A single struct covers
// every message type; message_type selects which fields are meaningful.
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

// toEvent maps a decoded line to an ExtractTreeEvent. ok is false for an unknown
// message_type, which the consumer skips (forward-compat with future restic).
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

// ExtractTree streams `restic restore <snap>[:<source>] --target <dir>
// --overwrite never --json` plus the include selection (a single argv
// `--include`, or for the multi-include diff shape an fd-4 pattern file with
// argv spillover — see buildExtractTreeArgs) and hands each parsed progress
// message to onEvent. With params.IncludePaths set, restic restores only the
// matching nodes (and their reconstructed parent dirs) metadata-faithfully —
// the single-file path and the diff extract's changed-paths-only restore.
// onEvent returning a non-nil error cancels the run via the child context and
// that error is surfaced verbatim (mirrors StreamSnapshotTree / StreamDiff).
//
// This layer does NOT impose a timeout: a real extract can run far longer than
// resticx's 2-minute default, so the deadline is the caller's responsibility
// (app.Extract bounds ctx with the configured extract_timeout). The child
// context here exists only so an onEvent error can stop restic.
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
		// The multi-include shape: the pattern payload rides fd 4, referenced in
		// the argv as /dev/fd/4.
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
		// A caller cancel beats every other classification.
		return context.Canceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return c.classify(ctx, "restore", runErr, c.sanitizeExtractStderr(stderr, params))
	case st.cbErr != nil:
		// onEvent asked to stop: surface its error verbatim so the caller can
		// distinguish its own failure from a restic failure.
		return st.cbErr
	case st.decodeErr != nil:
		return &Error{Kind: KindParse, Op: "restore", wrapped: st.decodeErr}
	case runErr == nil:
		if st.sawError {
			// Forward-compat: an "error" line reached stdout though restic still
			// exited 0 — treat as a partial restore.
			return &Error{Kind: KindPartial, Op: "restore"}
		}
		return nil
	case st.sawSummary:
		// restic emitted its final summary (RestoreTo completed and Finish ran)
		// yet exited non-zero: a partial restore. The failed items' per-item
		// errors went to stderr, which we deliberately do not surface. exit 1 is
		// restic's generic code, so the seen summary is the disambiguator.
		return &Error{Kind: KindPartial, Op: "restore", Code: exitCodeOf(runErr)}
	default:
		return c.classify(ctx, "restore", runErr, c.sanitizeExtractStderr(stderr, params))
	}
}

// buildExtractTreeArgs assembles the restore argv and is the single assertion
// surface for the safety invariants (00-framework.md §5). It emits, in order:
// --no-lock (a lock would be a write), --no-cache when NoCache is set, restore,
// the bare snapshot or <snap>:<source>, --target <abs>, --overwrite never
// (never clobber existing files), --json, and the include selection. It never
// emits --path, --delete, or "latest". Each include value is the RAW path run
// through literalIncludePattern so restic matches it as a literal, not a glob —
// the raw paths are what get validated.
//
// Include delivery: a single include keeps the historical `--include <pattern>`
// argv form. A multi-include run splits per fileSafePattern — file-safe
// patterns become the returned newline-joined pattern payload (delivered on
// fd 4; the argv carries `--include-file /dev/fd/4`), the rest stay as argv
// `--include` flags in caller order under the maxArgvIncludeBytes budget. The
// backend -o options are prepended by ExtractTree, not here, so these argv
// tests stay free of backend noise.
func buildExtractTreeArgs(p ExtractTreeParams) (args []string, patterns []byte, err error) {
	if err := assertCleanSnapshotID(p.SnapshotID); err != nil {
		return nil, nil, err
	}
	if err := assertCleanSource(p.Source); err != nil {
		return nil, nil, err
	}
	for _, inc := range p.IncludePaths {
		if inc == "" { // a present entry must be a real path; "" is only valid as "no list"
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
		// A literal ":" join, no shell involved: restic reads <snap>:<source> as
		// a single argv element, so metacharacters in source are inert bytes.
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
		// The canonical single-node extract: one literal-escaped argv include.
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

// fileSafePattern reports whether a raw literal include path can ride the fd-4
// pattern file. restic parses pattern files line-by-line, skips blank and
// #-comment lines, trims surrounding whitespace, and expands environment
// variables in every line (same rules as backup exclude files) — so
// structure-bearing bytes must stay on argv, where the pattern is one exact
// element: '$' (env-expanded; verified live — a file named 'weird$NAME file'
// is unmatchable through a pattern file), newline / CR (line structure), and
// leading/trailing whitespace (trimmed away). A leading '#' cannot occur:
// validated include paths are rooted at '/'.
func fileSafePattern(p string) bool {
	return !strings.ContainsAny(p, "$\n\r") && p == strings.TrimSpace(p)
}

// literalIncludePattern backslash-escapes restic's filepath.Match metacharacter
// set (\ [ ] * ?) in path so each component becomes a literal match, leaving "/"
// separators intact. With this, restic's --include matches exactly the supplied
// path instead of treating a real filename like a*.conf or backup[1].txt as a
// glob. NOT Windows-safe: Go's filepath.Match disables backslash escaping on
// Windows (treats "\" as a separator), which is why the app layer refuses the
// publish path on non-(linux|darwin) platforms before this ever runs.
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

// assertCleanIncludePath asserts the IncludePath contract: "" is allowed (no
// include filter); otherwise it must be NUL-free, rooted, equal to its
// model.CleanBrowsePath form, and non-root. Unlike assertCleanSource, "/" is
// rejected — an include of "/" is too broad and contradicts the one-file
// invariant the include filter exists to enforce. It validates the RAW path,
// never the escaped pattern.
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

// assertCleanSnapshotID rejects anything that is not restic's full, concrete
// snapshot ID: exactly snapshotIDHexLen lowercase-hex chars. Requiring the full
// length (not a prefix) is what keeps "latest", a short ID, and any other
// non-hex reference out of the argv — per 00-framework.md §5, restic must
// receive the verified long ID, never an ambiguous prefix it resolves itself.
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

// assertCleanSource asserts the model.CleanBrowsePath contract instead of
// re-applying it: source is "" or "/" (the whole-snapshot case), or it already
// equals its cleaned form (rooted, no "."/".." segments, no double slashes) and
// carries no NUL. Unclean input is refused, not repaired.
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

// sanitizeExtractStderr prepares restic restore stderr to be safe inside a
// returned *Error by scrubbing the source/target/include fragments. The shared
// scrubExtractStderr does the redact/mask/residual-path work. IncludePaths are
// scrubbed too because restic echoes the real selected paths (never the escaped
// patterns); under the file mapping the selected file's basename lives only in
// the include (Source is empty for nested), so without it a bare-basename
// mention would slip past the residual-'/' guard.
func (c *Client) sanitizeExtractStderr(stderr []byte, p ExtractTreeParams) []byte {
	return c.scrubExtractStderr(stderr, extractPathFragments(p.Source, p.Target, p.IncludePaths))
}

// scrubExtractStderr makes restic stderr safe to embed in a returned *Error. It
// redacts secrets, masks the supplied path fragments the redactor does not know
// about, then applies a conservative residual-path guard: any '/' surviving the
// masking may be an un-enumerated path (a sibling item restic named, the repo
// URL, /dev/fd/3), which we cannot prove path-free, so the whole stderr is
// dropped. Only KindUnknown renders Stderr, so the lost detail is a deliberate
// privacy trade. classify re-runs the redactor on the result, which is a no-op on
// already-masked text.
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

// extractPathFragments lists the path strings to scrub from stderr: the full
// source, target, and include paths plus their basenames (restic often reports
// just the leaf or the staging-dir name). The longer fragments are listed first
// so a basename is only matched where the full path did not already cover it.
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

// exitCodeOf extracts restic's exit code from a run error, or 0 if the error is
// not an exit error.
func exitCodeOf(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 0
}

// extractTreeStream decodes the restore NDJSON stream and forwards each event to
// onEvent. It keeps no event data: only the first callback/decode error, and
// whether a summary / error line was seen (the partial-restore signals
// ExtractTree classifies on).
type extractTreeStream struct {
	onEvent func(ExtractTreeEvent) error
	cancel  context.CancelFunc

	cbErr      error // onEvent's error, surfaced verbatim
	decodeErr  error // a genuine JSON decode failure (not a clean EOF)
	sawSummary bool
	sawError   bool
}

// consume reads the NDJSON stream and calls onEvent for each known message. An
// onEvent error records cbErr, cancels restic so it stops producing, and
// returns. Unknown message types are skipped; a clean EOF returns nil; a genuine
// decode error is recorded and returned for ExtractTree to classify as KindParse.
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
