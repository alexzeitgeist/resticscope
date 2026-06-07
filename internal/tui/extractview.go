package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"resticscope/internal/app"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// extractview.go renders extractView. Each state has its own body; the root
// Model's View() composes them with the existing header + footer scaffolding
// the same way browseHeaderView / browseBody do. Headings match the canonical
// mockups in 00-framework.md §14 byte-for-byte where layout allows.
//
// Privacy: the only path values rendered here are the ones the sub-model
// already holds for the lifetime of the modal — req.Source, m.staging, m.final,
// and (only when StagingCreated=true) m.result.StagingDir / FinalDir.
// extract.go's clearTransient zeroes all of those on every back-to-browse exit.

// extractHeaderView is the top header line on the extract view.
func (m Model) extractHeaderView() string {
	w, _ := m.effSize()
	left := m.styles.title.Render(extractTitle(m.extract))
	right := m.styles.dim.Render(extractHeaderHint(m.extract))
	return clip(m.spread(left, right), w)
}

// extractTitle picks the title label for the current sub-state.
func extractTitle(em extractModel) string {
	switch em.state {
	case extractStatePreview:
		if em.req.Mode == app.ExtractDirectoryTree {
			return "extract: dry-run preview"
		}
		return "extract: review"
	case extractStateRunning:
		return "extract: running"
	case extractStateSuccess:
		return "extract: done"
	case extractStateCanceled:
		return "extract: canceled"
	case extractStateError:
		return "extract: error"
	case extractStateFilePicker:
		return "extract: choose target root"
	case extractStateKeepDelete:
		return "extract: cleanup"
	default:
		// review — include the repo for orientation.
		if em.req.Repo != "" {
			return "extract: " + em.req.Repo
		}
		return "extract"
	}
}

// extractHeaderHint is the right-hand dim helper line in the header.
func extractHeaderHint(em extractModel) string {
	switch em.state {
	case extractStateRunning:
		return "esc cancel"
	case extractStateSuccess, extractStateCanceled, extractStateError:
		return "enter back"
	default:
		return "esc back"
	}
}

// extractBody returns the rendered body for the current sub-state. The root
// Model's View() pastes this between the header and the footer the same way
// browseBody does for browseView.
func (m Model) extractBody() string {
	w, _ := m.effSize()
	switch m.extract.state {
	case extractStateReview:
		return m.extractReviewBody(w)
	case extractStatePreview:
		if m.extract.req.Mode == app.ExtractDirectoryTree {
			return m.extractDirPreviewBody(w)
		}
		return m.extractFileReviewBody(w)
	case extractStateRunning:
		return m.extractRunningBody(w)
	case extractStateSuccess:
		return m.extractSuccessBody(w)
	case extractStateCanceled, extractStateError:
		return m.extractTerminalBody(w)
	case extractStateFilePicker:
		return m.extractFilePickerBody(w)
	case extractStateKeepDelete:
		return m.extractKeepDeleteBody(w)
	}
	return ""
}

// extractReviewBody renders the labeled "what will happen" screen — file source
// or directory source. Matches §14 review mockups.
func (m Model) extractReviewBody(w int) string {
	em := m.extract
	rows := []extractRow{
		{label: "Repo", value: em.req.Repo},
		{label: "Snapshot", value: extractSnapshotLine(em.req)},
		{label: "Source", value: em.req.Source},
		{label: "Type", value: extractTypeLine(em)},
		{label: "Output", value: extractOutputLine(em.req.Mode)},
	}
	if em.req.Mode == app.ExtractFileBytes {
		rows = append(rows, extractRow{label: "Metadata", value: "not preserved"})
	}
	rows = append(rows,
		extractRow{}, // spacer
		extractRow{label: "Target", value: extractTargetValue(em.final, extractValueWidth(w))},
		extractRow{label: "Overwrite", value: "never"},
	)
	body := renderExtractRows(m, rows, w)
	if em.dryRunning {
		// A restic restore --dry-run is in flight; the screen stays on review
		// but advertises the wait instead of the enter/target hint.
		return body + "\n\n" + clip(m.styles.meta.Render("  running dry-run…"), w)
	}
	hint := m.styles.meta.Render("  enter ▶ ")
	if em.req.Mode == app.ExtractDirectoryTree {
		hint += m.styles.meta.Render("dry-run preview")
	} else {
		hint += m.styles.meta.Render("review")
	}
	return body + "\n\n" + clip(hint, w)
}

// extractFileReviewBody is the app-side commit-time review screen for file
// sources (no restic call); enter→preview took us here, g commits.
func (m Model) extractFileReviewBody(w int) string {
	em := m.extract
	rows := []extractRow{
		{label: "Source", value: em.req.Source},
		{label: "Type", value: extractTypeLine(em)},
		{label: "Output", value: "file bytes (no metadata)"},
		{label: "Overwrite", value: "never"},
		{},
		// Bytes land at <staging>/<name> and rename to <final>/<name>, so show
		// the file path the user gets, not the parent directory. Wrap rather than
		// elide (framework §8) so a long path is fully visible.
		{label: "Staging", value: wrapPathValue(filepath.Join(em.staging, em.req.SourceName), extractValueWidth(w))},
		{label: "Final", value: wrapPathValue(filepath.Join(em.final, em.req.SourceName), extractValueWidth(w))},
	}
	body := renderExtractRows(m, rows, w)
	hint := m.styles.meta.Render("  g ▶ extract")
	return body + "\n\n" + clip(hint, w)
}

// extractDirPreviewBody renders the scrollable dry-run preview list.
func (m Model) extractDirPreviewBody(w int) string {
	em := m.extract
	header := clip(m.styles.meta.Render("  "+extractPreviewHeader(em)), w)
	summary := clip(m.styles.meta.Render("  Summary  "+extractPreviewSummaryLine(em.previewSummary)), w)
	rows := em.previewRows
	if len(rows) == 0 {
		empty := clip(m.styles.meta.Render("  (no entries)"), w)
		return strings.Join([]string{header, summary, "", empty}, "\n")
	}
	// Reserve four lines (header, summary, blank, scroll-note) above the rows;
	// the rest is the line budget. Rows may wrap to several lines each, so fill
	// the budget line-by-line from the (clamped) top row rather than assuming one
	// line per row — the clamp guarantees the chosen start fills the last page.
	_, h := m.effSize()
	budget := m.extractPreviewVisible()
	start := clampPreviewOffset(em.previewScrollOffset, rows, w, h)
	lines := make([]string, 0, budget+4)
	lines = append(lines, header, summary, "")
	used, end := 0, start
	for end < len(rows) && used < budget {
		rl := renderExtractPreviewRowLines(rows[end], w)
		if used+len(rl) > budget && used > 0 {
			break // this row won't fit and we've shown at least one; stop cleanly
		}
		for _, ln := range rl {
			if used >= budget {
				break // a single oversized row is capped, never overflowing the pane
			}
			lines = append(lines, clip(ln, w))
			used++
		}
		end++
	}
	if start > 0 || end < len(rows) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(rows))), w))
	}
	return strings.Join(lines, "\n")
}

// extractPreviewHeader is the "<source> → <final>  <overwrite>" line above the
// summary, matching the mockup.
func extractPreviewHeader(em extractModel) string {
	return em.req.Source + " → " + collapsePath(em.final) + "  never"
}

// extractPreviewSummaryLine flattens the partial result fields into a single
// "N files · M dirs · X · 0 skipped" line.
func extractPreviewSummaryLine(r app.ExtractResult) string {
	return fmt.Sprintf("%d files · %d dirs · %s · 0 skipped",
		r.Files, r.Dirs, humanize.Bytes(r.Bytes))
}

// renderExtractPreviewRowLines renders one dry-run preview item as one or more
// lines: the action prefix and the item path on the first line, with the size
// right-aligned when it fits, and any wrapped path remainder on indented
// continuation lines. The path is wrapped (framework §14 / step 05), never
// elided — this is the screen where the user inspects exactly what will be
// extracted, so a deep path must stay fully readable.
func renderExtractPreviewRowLines(item app.ExtractPreviewItem, w int) []string {
	if w <= 0 {
		w = defaultWidth
	}
	prefix := actionPrefix(item.Action)
	const pathCol = 4 // "  " indent + prefix(1) + space(1)
	avail := w - pathCol
	if avail < 1 {
		avail = 1
	}
	chunks := strings.Split(wrapPathValue(item.Item, avail), "\n")
	lines := make([]string, 0, len(chunks))
	for i, ch := range chunks {
		if i == 0 {
			lines = append(lines, "  "+prefix+" "+ch)
		} else {
			lines = append(lines, strings.Repeat(" ", pathCol)+ch)
		}
	}
	// Right-align the size on the first line, but only when it fits without
	// overlapping the path; otherwise drop it — never elide the path to fit it.
	if item.Size > 0 {
		size := humanize.Bytes(item.Size)
		if first := lines[0]; len([]rune(first))+2+len([]rune(size)) <= w {
			pad := w - len([]rune(first)) - len([]rune(size))
			lines[0] = first + strings.Repeat(" ", pad) + size
		}
	}
	return lines
}

// actionPrefix maps the restore action to its one-character prefix glyph.
func actionPrefix(action model.RestoreAction) string {
	switch action {
	case model.RestoreActionRestored:
		return "+"
	case model.RestoreActionMetadata:
		return "~"
	case model.RestoreActionSkipped:
		return "-"
	default:
		return " "
	}
}

// extractRunningBody renders the live-progress screen.
func (m Model) extractRunningBody(w int) string {
	em := m.extract
	body := renderExtractRows(m, []extractRow{
		{label: "Source", value: em.req.Source},
		{label: "Staging", value: collapsePath(em.staging)},
	}, w)
	// Progress bar + status line.
	pct, hasPct := extractPercent(em)
	var pctLabel string
	if hasPct {
		pctLabel = fmt.Sprintf("  %3d%%", int(pct*100))
	} else {
		pctLabel = "    —"
	}
	// Scale the bar to the pane: cap it at 48 so a wide terminal doesn't draw an
	// ungainly full-width bar (keeping the canonical mockup's look at >=58 cols),
	// and floor it at 16 so the trailing percentage stays visible when narrow.
	barW := w - 2 - len([]rune(pctLabel)) - 2 // leading indent + label + slack
	if barW > 48 {
		barW = 48
	}
	if barW < 16 {
		barW = 16
	}
	bar := renderProgressBar(barW, pct, hasPct)
	barLine := clip("  "+bar+pctLabel, w)
	statusLine := clip("  "+m.styles.meta.Render(extractRunningStatus(em)), w)
	return body + "\n\n" + barLine + "\n\n" + statusLine
}

// extractPercent computes the bar fraction; hasPct is false when
// BytesTotal == 0 (file mode, or pre-first-report).
func extractPercent(em extractModel) (float64, bool) {
	if em.progress.BytesTotal <= 0 {
		return 0, false
	}
	pct := float64(em.progress.BytesDone) / float64(em.progress.BytesTotal)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	return pct, true
}

// renderProgressBar returns a width-w bar built from the same characters used
// in the canonical mockup. When pct is unknown (indeterminate) the bar shows a
// dim baseline with a moving block; for simplicity in v1 we just render a dim
// row of light shade characters.
func renderProgressBar(width int, pct float64, has bool) string {
	if width < 4 {
		width = 4
	}
	if !has {
		return strings.Repeat("░", width)
	}
	fill := int(float64(width) * pct)
	if fill < 0 {
		fill = 0
	}
	if fill > width {
		fill = width
	}
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill)
}

// extractRunningStatus is the single-line status under the progress bar.
func extractRunningStatus(em extractModel) string {
	parts := []string{}
	p := em.progress
	// Bytes done / total (only with a total).
	if p.BytesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s / %s", humanize.Bytes(p.BytesDone), humanize.Bytes(p.BytesTotal)))
	} else {
		parts = append(parts, fmt.Sprintf("%s / —", humanize.Bytes(p.BytesDone)))
	}
	if p.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d / %d files", p.FilesDone, p.FilesTotal))
	} else if p.FilesDone > 0 {
		parts = append(parts, fmt.Sprintf("%d files", p.FilesDone))
	}
	if em.rate > 0 {
		parts = append(parts, fmt.Sprintf("%s/s", humanize.Bytes(int64(em.rate))))
	}
	if p.SecondsRemaining > 0 {
		parts = append(parts, "eta "+humanize.Duration(time.Duration(p.SecondsRemaining)*time.Second))
	}
	return strings.Join(parts, " · ")
}

// extractSuccessBody renders the post-rename congratulation page.
func (m Model) extractSuccessBody(w int) string {
	em := m.extract
	ok := m.styles.good.Render("✓ ")
	summary := ok + fmt.Sprintf("extracted %d files · %d dirs · %s · in %s",
		em.result.Files, em.result.Dirs,
		humanize.Bytes(em.result.Bytes),
		humanize.Duration(em.result.Elapsed))
	body := []string{
		"  " + summary,
		"",
		"  " + m.styles.label.UnsetWidth().Render("Target"),
		"    " + m.styles.meta.Render(em.result.FinalDir),
		"",
		"    " + m.styles.dim.Render("s     open a shell in the target directory"),
		"    " + m.styles.dim.Render("enter back to browse"),
	}
	return clipLines(body, w)
}

// extractTerminalBody renders the canceled / error screen with the
// keep-or-delete prompt when staging exists.
func (m Model) extractTerminalBody(w int) string {
	em := m.extract
	var headline string
	switch em.state {
	case extractStateCanceled:
		headline = m.styles.bad.Render("! ") + extractCancelHeadline(em)
	default:
		headline = m.styles.errText.Render("✕ ") + extractErrorHeadline(em)
	}
	lines := []string{"  " + headline}
	if em.result.StagingCreated && em.result.StagingDir != "" && stagingDirExists(em.result.StagingDir) {
		lines = append(lines,
			"",
			"  "+m.styles.meta.Render("Staging output (extract did not complete):"),
		)
		// Wrap the staging path onto indented lines (framework §14) so the path the
		// user keeps or deletes is fully visible rather than clip-truncated.
		for _, ln := range strings.Split(wrapPathValue(em.result.StagingDir, w-4), "\n") {
			lines = append(lines, "    "+m.styles.meta.Render(ln))
		}
		lines = append(lines,
			"",
			"  "+m.styles.meta.Render("What should resticscope do with the staging dir?"),
			"    "+m.styles.dim.Render("k     keep as-is  (default)"),
			"    "+m.styles.dim.Render("d     delete the staging dir"),
		)
	} else if em.err != nil {
		lines = append(lines,
			"",
			"    "+m.styles.dim.Render("enter back to browse"),
		)
	}
	return clipLines(lines, w)
}

// extractCancelHeadline composes the "! canceled at … bytes written" line.
func extractCancelHeadline(em extractModel) string {
	p := em.progress
	parts := []string{"canceled"}
	if p.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("at %d / %d files", p.FilesDone, p.FilesTotal))
	} else if p.FilesDone > 0 {
		parts = append(parts, fmt.Sprintf("at %d files", p.FilesDone))
	}
	if p.BytesDone > 0 {
		parts = append(parts, humanize.Bytes(p.BytesDone)+" written")
	}
	return strings.Join(parts, " · ")
}

// extractErrorHeadline composes the path-free error line shown on
// extractStateError. The underlying error has already been sanitized by the
// app / resticx layer per the privacy contract.
func extractErrorHeadline(em extractModel) string {
	if em.err == nil {
		return "extract failed"
	}
	return firstLine(em.err.Error())
}

// extractKeepDeleteBody mirrors extractTerminalBody during the brief window
// between the user pressing `d` and the RemoveAll done message.
func (m Model) extractKeepDeleteBody(w int) string {
	return clipLines([]string{
		"  " + m.styles.meta.Render("deleting staging directory…"),
	}, w)
}

// extractFilePickerBody renders the embedded filepicker overlay.
func (m Model) extractFilePickerBody(w int) string {
	em := m.extract
	header := clip(m.styles.meta.Render("  "+em.filepicker.CurrentDirectory), w)
	body := em.filepicker.View()
	out := []string{header, "", body}
	if em.filepickerErr != "" {
		out = append(out, "", clip(m.styles.errText.Render("  "+em.filepickerErr), w))
	}
	return strings.Join(out, "\n")
}

// extractRow is a single labeled row for the labeled-field screens (review,
// running). value may contain "\n" for multi-line right-hand text.
type extractRow struct {
	label string
	value string
}

// renderExtractRows produces the labeled-field block used by review and running
// screens. A row with an empty label and empty value becomes a blank line.
func renderExtractRows(m Model, rows []extractRow, w int) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.label == "" && r.value == "" {
			out = append(out, "")
			continue
		}
		labelCell := m.styles.label.Render(r.label)
		vlines := strings.Split(r.value, "\n")
		first := clip("  "+labelCell+"  "+m.styles.name.UnsetWidth().Render(vlines[0]), w)
		out = append(out, first)
		indent := strings.Repeat(" ", 2+labelWidth+2)
		for _, v := range vlines[1:] {
			out = append(out, clip(indent+m.styles.meta.Render(v), w))
		}
	}
	return strings.Join(out, "\n")
}

// extractSnapshotLine renders the snapshot short + (long…) form per the mockup.
func extractSnapshotLine(req app.ExtractRequest) string {
	short := req.SnapshotShort
	if short == "" && len(req.SnapshotID) >= 8 {
		short = req.SnapshotID[:8]
	}
	long := req.SnapshotID
	if len(long) > 20 {
		long = long[:20] + "…"
	}
	return fmt.Sprintf("%s  (%s)", short, long)
}

// extractTypeLine renders the "file · 412 B" / "directory · 4.2 MiB" cell. The
// size is the originating BrowseEntry's (a directory's recursive subtree size),
// shown only when known; per-entry counts are omitted in v1 since the request
// doesn't carry them.
func extractTypeLine(em extractModel) string {
	kind := "directory"
	if em.req.Mode == app.ExtractFileBytes {
		kind = "file"
	}
	if em.srcSize > 0 {
		return kind + " · " + humanize.Bytes(em.srcSize)
	}
	return kind
}

// extractOutputLine names the output shape.
func extractOutputLine(mode app.ExtractMode) string {
	if mode == app.ExtractFileBytes {
		return "file bytes"
	}
	return "directory tree"
}

// extractValueWidth is the cell budget for a labeled row's value: the full width
// less the gutter (2), the fixed label column (labelWidth), and the label/value
// gap (2). It matches renderExtractRows's own layout so a pre-wrapped value's
// lines each fit and clip is left a no-op.
func extractValueWidth(w int) int {
	return w - 2 - labelWidth - 2
}

// extractTargetValue renders the final dir for the review screen: split at the
// last "/" into the parent root and the per-op subdir (the mockup's two-line
// shape), then wrap either segment that still exceeds avail so a long target is
// laid out across indented lines rather than elided (framework §8).
func extractTargetValue(final string, avail int) string {
	idx := strings.LastIndex(final, "/")
	if idx <= 0 {
		return wrapPathValue(final, avail)
	}
	head := wrapPathValue(final[:idx+1], avail)
	tail := wrapPathValue(final[idx+1:], avail-2)
	return head + "\n  " + tail
}

// wrapPathValue splits p into consecutive runs of at most avail cells so a long
// path renders across multiple "\n"-joined lines instead of being truncated. A
// path that already fits (or a non-positive avail) is returned unchanged.
// Splitting on runes keeps multibyte characters intact; renderExtractRows still
// clips each resulting line as a final safety net.
func wrapPathValue(p string, avail int) string {
	if avail <= 0 {
		return p
	}
	r := []rune(p)
	if len(r) <= avail {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(r); i += avail {
		if i > 0 {
			b.WriteByte('\n')
		}
		end := i + avail
		if end > len(r) {
			end = len(r)
		}
		b.WriteString(string(r[i:end]))
	}
	return b.String()
}

// collapsePath shortens a long absolute path by leading "…/". Cheap and good
// enough for the running-state staging field, where the full path is too long
// to fit on a single line.
func collapsePath(p string) string {
	const max = 60
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	// Slice on a rune boundary so a multi-byte path component (Unicode dir or
	// snapshot names) is never split mid-rune into invalid UTF-8.
	return "…" + string(r[len(r)-max+1:])
}

// extractPreviewVisible reserves rows above and below the preview list for
// header, summary, blank, and scroll-note (4 lines plus the surrounding
// header/footer the root View() already accounts for).
func (m Model) extractPreviewVisible() int {
	_, h := m.effSize()
	return extractPreviewVisibleAt(h)
}

// extractPreviewVisibleAt is the height-only form of extractPreviewVisible, used
// by the scroll handler (which holds the synced terminal height but not the root
// Model). Both agree on the window size so the offset never climbs past the
// top-anchored window's max start and strands the first Up presses. The extract
// footer is always a single help line, so the footer reservation is the constant
// 1 rather than a measured footerRows().
func extractPreviewVisibleAt(h int) int {
	if h <= 0 {
		h = defaultHeight
	}
	overhead := headerRows + 2*gapRows + 1 + 4 // +1 footer, +4 preview scaffolding
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

// clampPreviewOffset bounds a preview scroll offset so the last page pins to the
// bottom. Because rows can wrap to several lines, the largest useful top row is
// the earliest one that — taken with everything below it — still fits one budget
// of lines; it is found by walking up from the last row and summing real line
// heights. This keeps the final page full (no scroll dead-zone) while staying
// reachable for tall rows, where a naive "len - visibleRows" bound would either
// strand the bottom rows or leave a dead-zone. For one-line rows it reduces to
// len - budget, matching the simple case.
func clampPreviewOffset(off int, rows []app.ExtractPreviewItem, w, height int) int {
	if off < 0 {
		off = 0
	}
	n := len(rows)
	if n == 0 {
		return 0
	}
	if off > n-1 {
		off = n - 1
	}
	budget := extractPreviewVisibleAt(height)
	used, maxStart := 0, n-1
	for i := n - 1; i >= 0; i-- {
		hgt := len(renderExtractPreviewRowLines(rows[i], w))
		if used+hgt > budget {
			break
		}
		used += hgt
		maxStart = i
	}
	if off > maxStart {
		off = maxStart
	}
	return off
}

// clipLines clips each line in s to w cells and joins them with "\n".
func clipLines(s []string, w int) string {
	out := make([]string, len(s))
	for i, ln := range s {
		out[i] = clip(ln, w)
	}
	return strings.Join(out, "\n")
}
