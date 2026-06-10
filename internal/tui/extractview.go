package tui

import (
	"fmt"
	"strings"

	"resticscope/internal/app"
	"resticscope/internal/humanize"
)

// extractview.go renders extractView. Each state has its own body; the root
// Model's View() composes them with the existing header + footer scaffolding
// the same way browseHeaderView / browseBody do. Headings match the canonical
// mockups in 00-framework.md §14 byte-for-byte where layout allows.
//
// Privacy: the only path values rendered here are the ones the sub-model
// already holds for the lifetime of the modal — req.Source, m.staging, m.final,
// and (after a clean publish) m.result.FinalDir / FinalPath / StagingDir. The
// root model drops the whole sub-model on every back-to-browse exit, so all of
// those are zeroed.
//
// Note m.final / FinalPath now EMBED the source path (the mirror layout puts the
// source's true path under the snapshot dir), so they appear only in the review
// and success bodies — the user's own in-progress / completed action — never in
// the error / cancel terminal bodies or logs. The terminal bodies show only the
// staging path (which carries just the sanitized basename) plus a path-free hint.
// (logExtractSuccess stays path-free — snapshot short + counts only.)

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
		// review — include the repo and short snapshot id for orientation,
		// mirroring browse's "browse: <repo> · <shortid>" header.
		if em.req.Repo == "" {
			return "extract"
		}
		title := "extract: " + em.req.Repo
		if short := extractShortSnap(em.req); short != "" {
			title += " · " + short
		}
		return title
	}
}

// extractHeaderHint is the right-hand dim helper line in the header.
func extractHeaderHint(em extractModel) string {
	switch em.state {
	case extractStateRunning:
		return "q cancel"
	case extractStateSuccess, extractStateCanceled, extractStateError:
		return "enter back"
	default:
		return "q back"
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

// extractReviewBody renders the single labeled "what will happen" screen for both
// file and directory sources. enter commits straight to the live extract (there
// is no dry-run preview step). Both modes mirror the source to its true path under
// the per-snapshot directory, so one Target row (em.final, the exact mirror path)
// serves both. Matches §14 review mockups.
func (m Model) extractReviewBody(w int) string {
	em := m.extract
	// Repo + short snapshot id live in the header title; the source size is folded
	// into the Source row, so Type is gone. The key hints live in the footer only.
	// Target wraps rather than elides (framework §8) so a long mirror path is
	// fully visible.
	rows := []extractRow{
		{label: "Source", value: extractSourceValue(em)},
		{label: "Output", value: extractOutputValue(em)},
		{}, // spacer
		{label: "Target", value: extractTargetValue(em.final, extractValueWidth(w))},
	}
	body := renderExtractRows(m.styles, rows, w)
	// Path-free sudo notice (auth failed / privileged unavailable) under the rows.
	if em.reviewNotice != "" {
		body += "\n\n" + clip("  "+m.styles.errText.Render(em.reviewNotice), w)
	}
	return body
}

// extractRunningBody renders the live-progress screen.
func (m Model) extractRunningBody(w int) string {
	em := m.extract
	body := renderExtractRows(m.styles, []extractRow{
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
// BytesTotal == 0 (pre-first-report — restic restore reports total_bytes for
// both file and directory extracts once it starts).
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
	if em.rate.rate > 0 {
		parts = append(parts, fmt.Sprintf("%s/s", humanize.Bytes(int64(em.rate.rate))))
	}
	// No ETA segment: restic restore's JSON reports no seconds_remaining.
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
		"    " + m.styles.meta.Render(em.result.FinalPath),
	}
	if em.req.Privileged {
		body = append(body,
			"",
			"  "+m.styles.dim.Render("extracted as root — snapshot file ownership preserved"),
		)
	}
	// Count-only warning when the tree carried unsafe symlinks. No names — only the
	// count — so the line stays path-free even though FinalPath is shown above.
	if em.result.UnsafeSymlinks > 0 {
		body = append(body,
			"",
			"  "+m.styles.bad.Render("! ")+m.styles.dim.Render(extractUnsafeSymlinkWarning(em.result.UnsafeSymlinks, em.cfg.UnsafeSymlinks)),
		)
	}
	body = append(body,
		"",
		"    "+m.styles.dim.Render("s     open a shell in the target directory"),
		"    "+m.styles.dim.Render("enter back to browse"),
	)
	return clipLines(body, w)
}

// extractUnsafeSymlinkWarning composes the success-screen warning for unsafe
// symlinks, phrased for the [extract] unsafe_symlinks policy that applied (the
// sub-model's own validated config — policy is config-only, never per-request).
// It carries only the count, never a path or a link name.
func extractUnsafeSymlinkWarning(n int, policy string) string {
	switch policy {
	case "skip":
		return fmt.Sprintf("%d unsafe symlinks removed from the output.", n)
	case "placeholder":
		return fmt.Sprintf("%d unsafe symlinks replaced with inert text files recording their target.", n)
	default: // keep (and any unknown/empty policy)
		return fmt.Sprintf("%d unsafe symlinks left in place — targets are absolute or outside the extracted tree and alias your live filesystem; inspect before use.", n)
	}
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
	if em.stagingExists {
		// Staging keep-or-delete is the action here; the refusal hint (which points
		// at the `t` retarget key) is deliberately omitted — the user must resolve
		// the staging dir first, and handleTerminalKey does not honor `t` in this
		// branch (it would orphan the staging dir).
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
		// No staging to resolve: surface the actionable refusal hint (only for an
		// occupied-target/staging refusal — handleTerminalKey honors `t` here), then
		// the back affordance.
		if hint := extractRefusalHint(em); hint != "" {
			lines = append(lines, "", "  "+m.styles.dim.Render(hint))
		}
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

// extractRefusalHint returns a path-free, one-line resolution hint for the
// terminal body when the extract was refused because the target (or staging) path
// was already occupied — the merge only ever fills empty space, so the user must
// pick a different target or clear the occupant. The `t` key it names is honored
// by handleTerminalKey in the same (no-staging) branch this hint renders in, so it
// is never a dead key. Pure TUI guidance: it names no path (the app-layer error is
// path-free too). Empty for any other outcome, so a cancel or a genuine IO error
// shows no hint.
func extractRefusalHint(em extractModel) string {
	if isExtractRefusal(em.err) {
		return "press t to choose another target, or remove the existing output"
	}
	return ""
}

// extractKeepDeleteBody mirrors extractTerminalBody during the brief window
// between the user pressing `d` and the RemoveAll done message.
func (m Model) extractKeepDeleteBody(w int) string {
	return clipLines([]string{
		"  " + m.styles.meta.Render("deleting staging directory…"),
	}, w)
}

// extractFilePickerBody renders the embedded filepicker overlay. The picker's
// View() is clipped line-by-line to the body width: bubbles' filepicker has no
// width concept (no SetWidth/AutoWidth as of v2.1.0), so a long entry name
// would otherwise overflow past the layout boundary.
func (m Model) extractFilePickerBody(w int) string {
	em := m.extract
	header := clip(m.styles.meta.Render("  "+em.filepicker.CurrentDirectory), w)
	body := clipLines(strings.Split(em.filepicker.View(), "\n"), w)
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
func renderExtractRows(st styles, rows []extractRow, w int) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.label == "" && r.value == "" {
			out = append(out, "")
			continue
		}
		labelCell := st.extractLabel.Render(r.label)
		vlines := strings.Split(r.value, "\n")
		// Values render in the terminal's default foreground (unstyled), matching the
		// browse list's entry names (browseRow) and the detail view's field values —
		// the bright "main content" color — rather than the dimmer cream of styles.name.
		first := clip("  "+labelCell+"  "+vlines[0], w)
		out = append(out, first)
		indent := strings.Repeat(" ", 2+labelWidth+2)
		for _, v := range vlines[1:] {
			out = append(out, clip(indent+v, w))
		}
	}
	return strings.Join(out, "\n")
}

// extractShortSnap returns the snapshot's short id for the header title,
// preferring the precomputed SnapshotShort and falling back to the first 8 chars
// of the full id (via shortID). Empty when neither is set.
func extractShortSnap(req app.ExtractRequest) string {
	if req.SnapshotShort != "" {
		return req.SnapshotShort
	}
	return shortID(req.SnapshotID)
}

// extractSourceValue renders the Source row: the snapshot source path with the
// originating entry's size in parentheses when known (a directory's recursive
// subtree size). The size is what the dropped Type row used to carry; it is shown
// only when known since the request doesn't carry per-entry counts.
func extractSourceValue(em extractModel) string {
	if em.srcSize > 0 {
		return em.req.Source + " (" + humanize.Bytes(em.srcSize) + ")"
	}
	return em.req.Source
}

// extractOutputLine names the output shape.
func extractOutputLine(mode app.ExtractMode) string {
	if mode == app.ExtractFile {
		return "file"
	}
	return "directory tree"
}

// extractOutputValue is the Output row: the shape plus the privileged marker
// when the `p` toggle is on (the restore then runs as root via sudo so the
// snapshot's file ownership is applied).
func extractOutputValue(em extractModel) string {
	out := extractOutputLine(em.req.Mode)
	if em.req.Privileged {
		out += " · as root (ownership preserved)"
	}
	return out
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

// clipLines clips each line in s to w cells and joins them with "\n".
func clipLines(s []string, w int) string {
	out := make([]string, len(s))
	for i, ln := range s {
		out[i] = clip(ln, w)
	}
	return strings.Join(out, "\n")
}
