package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Extraction rendering may show modal-lifetime paths. Terminal bodies directly
// render only owned staging for cleanup, not source-bearing final paths. Root
// exit zeroes the entire modal, and successful extraction logs omit paths.

// extractTitle labels modal state; navigation hints remain in the footer.
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
		// Diff review identifies the pair instead of one side.
		if em.req.Repo == "" {
			return "extract"
		}
		title := "extract: " + em.req.Repo
		if em.diff != nil {
			return title + " · " + em.diff.firstShort + " → " + em.diff.secondShort
		}
		if short := extractShortSnap(em.req); short != "" {
			title += " · " + short
		}
		return title
	}
}

// extractBody renders the current modal state.
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

// extractReviewBody previews the mirrored target before direct live commit.
func (m Model) extractReviewBody(w int) string {
	em := m.extract
	// Target paths wrap rather than elide; key hints remain in the footer.
	rows := []extractRow{
		{label: "Source", value: extractSourceValue(em)},
	}
	if em.diff != nil {
		// Diff changes come from filtered diff data, not whole-subtree counts.
		rows = append(rows,
			extractRow{label: "Diff", value: extractDiffPairValue(em)},
			extractRow{label: "Changes", value: extractDiffChangesValue(em)},
		)
	}
	if em.srcCountsKnown {
		// Indexed counts exclude the source directory itself, unlike completed totals.
		rows = append(rows, extractRow{label: "Contains", value: fmt.Sprintf("%s · %s",
			humanize.Count(em.srcFiles, "file", "files"),
			humanize.Count(em.srcDirs, "dir", "dirs"))})
	}
	rows = append(rows,
		extractRow{},
		extractRow{label: "Target", value: extractTargetValue(extractTargetPath(em), extractValueWidth(w), extractIsDir(em))},
	)
	body := renderExtractRows(m.styles, rows, w)
	// Surface advisory occupancy before the authoritative run-time check.
	if em.isTargetBusy {
		body += "\n\n" + clip("  "+m.styles.errText.Render("target already exists — choose another target or remove the existing output"), w)
	}
	// Free-space preflight is advisory; run-time errors remain authoritative.
	if extractSpaceShort(em) {
		body += "\n\n" + clip("  "+m.styles.errText.Render(fmt.Sprintf(
			"source may not fit the target filesystem — %s needed, %s free",
			humanize.Bytes(em.srcSize), humanize.Bytes(em.targetFree))), w)
	}
	if em.req.Privileged {
		body += "\n\n" + clip("  "+m.styles.dim.Render("as root — snapshot file ownership preserved"), w)
	}
	// Privilege status uses one slot for either progress or a path-free failure.
	if em.isSudoBusy {
		body += "\n\n" + clip("  "+m.styles.meta.Render("checking sudo access — the terminal may switch to a sudo password prompt"), w)
	} else if em.reviewNotice != "" {
		body += "\n\n" + clip("  "+m.styles.errText.Render(em.reviewNotice), w)
	}
	return body
}

// extractRunningBody shows progress and the active diff side when applicable.
func (m Model) extractRunningBody(w int) string {
	em := m.extract
	rows := []extractRow{
		{label: "Source", value: em.req.Source},
	}
	if em.diff != nil {
		rows = append(rows, extractRow{label: "Snapshot", value: extractDiffRunSideValue(em)})
	}
	rows = append(rows, extractRow{label: "Staging", value: collapsePath(em.staging)})
	body := renderExtractRows(m.styles, rows, w)
	pct, hasPct := extractPercent(em)
	var pctLabel string
	if hasPct {
		pctLabel = fmt.Sprintf("  %3d%%", int(pct*100))
	} else {
		pctLabel = "    —"
	}
	// Keep the bar between 16 and 48 cells so its percentage remains visible.
	barW := max(
		min(

			w-2-len([]rune(pctLabel))-2, 48), 16)
	bar := renderProgressBar(barW, pct, hasPct)
	barLine := clip("  "+bar+pctLabel, w)
	statusLine := clip("  "+m.styles.meta.Render(extractRunningStatus(em)), w)
	return body + "\n\n" + barLine + "\n\n" + statusLine
}

// extractPercent returns no percentage until restic reports a positive byte total.
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

// renderProgressBar uses light shading for indeterminate progress.
func renderProgressBar(width int, pct float64, has bool) string {
	if width < 4 {
		width = 4
	}
	if !has {
		return strings.Repeat("░", width)
	}
	fill := min(max(int(float64(width)*pct), 0), width)
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill)
}

func extractRunningStatus(em extractModel) string {
	parts := []string{}
	p := em.progress
	if p.BytesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s / %s", humanize.Bytes(p.BytesDone), humanize.Bytes(p.BytesTotal)))
	} else {
		parts = append(parts, humanize.Bytes(p.BytesDone)+" / —")
	}
	if p.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d / %s", p.FilesDone, humanize.Count(p.FilesTotal, "file", "files")))
	} else if p.FilesDone > 0 {
		parts = append(parts, humanize.Count(p.FilesDone, "file", "files"))
	}
	if em.rate.rate > 0 {
		parts = append(parts, humanize.Bytes(int64(em.rate.rate))+"/s")
	}
	// Restic restore JSON provides no remaining-time estimate.
	return strings.Join(parts, " · ")
}

// extractSuccessBody renders published output while leaving key hints to the footer.
func (m Model) extractSuccessBody(w int) string {
	em := m.extract
	if em.diff != nil {
		return m.extractDiffSuccessBody(w)
	}
	ok := m.styles.good.Render("✓ ")
	summary := ok + fmt.Sprintf("extracted %s · %s · %s · in %s",
		humanize.Count(em.result.Files, "file", "files"),
		humanize.Count(em.result.Dirs, "dir", "dirs"),
		humanize.Bytes(em.result.Bytes),
		humanize.Duration(em.result.Elapsed))
	body := []string{
		"  " + summary,
		"",
		"  " + m.styles.label.UnsetWidth().Render("Target"),
	}
	for ln := range strings.SplitSeq(wrapPathValue(em.result.FinalPath, w-4), "\n") {
		body = append(body, "    "+m.styles.meta.Render(ln))
	}
	if em.req.Privileged {
		body = append(body, "")
		body = append(body, m.extractDimNoteLines("extracted as root — snapshot file ownership preserved", w)...)
	}
	// Unsafe-symlink warnings reveal only counts, never names.
	if em.result.UnsafeSymlinks > 0 {
		body = append(body, "")
		body = append(body, m.extractWarnLines(extractUnsafeSymlinkWarning(em.result.UnsafeSymlinks, em.cfg.UnsafeSymlinks), w)...)
	}
	return clipLines(body, w)
}

// extractWarnLines wraps marked warnings with aligned continuation lines.
func (m Model) extractWarnLines(text string, w int) []string {
	lines := strings.Split(wrapWords(text, w-4), "\n")
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i == 0 {
			out = append(out, "  "+m.styles.bad.Render("! ")+m.styles.dim.Render(ln))
		} else {
			out = append(out, "    "+m.styles.dim.Render(ln))
		}
	}
	return out
}

// extractDimNoteLines wraps dim notes within the body gutter.
func (m Model) extractDimNoteLines(text string, w int) []string {
	lines := strings.Split(wrapWords(text, w-2), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, "  "+m.styles.dim.Render(ln))
	}
	return out
}

// extractUnsafeSymlinkWarning describes the configured policy using counts only.
func extractUnsafeSymlinkWarning(n int, policy string) string {
	count := humanize.Count(n, "unsafe symlink", "unsafe symlinks")
	switch policy {
	case config.UnsafeSymlinksSkip:
		return count + " removed from the output."
	case config.UnsafeSymlinksPlaceholder:
		if n == 1 {
			return count + " replaced with an inert text file recording its target."
		}
		return count + " replaced with inert text files recording their target."
	default: // keep (and any unknown/empty policy)
		if n == 1 {
			return count + " left in place — its target is absolute or outside the extracted tree and aliases your live filesystem; inspect before use."
		}
		return count + " left in place — targets are absolute or outside the extracted tree and alias your live filesystem; inspect before use."
	}
}

// extractDiffSuccessBody combines sequential side totals and targets their shared
// shell container, noting sides with nothing selected.
func (m Model) extractDiffSuccessBody(w int) string {
	em := m.extract
	var files, dirs, unsafe int
	var bytes int64
	var elapsed time.Duration
	for _, s := range em.published {
		files += s.result.Files
		dirs += s.result.Dirs
		bytes += s.result.Bytes
		unsafe += s.result.UnsafeSymlinks
		elapsed += s.result.Elapsed
	}
	summary := m.styles.good.Render("✓ ") + fmt.Sprintf("extracted %s · %s · %s · in %s",
		humanize.Count(files, "file", "files"),
		humanize.Count(dirs, "dir", "dirs"),
		humanize.Bytes(bytes),
		humanize.Duration(elapsed))
	body := []string{
		"  " + summary,
		"",
		"  " + m.styles.label.UnsetWidth().Render("Target"),
	}
	for ln := range strings.SplitSeq(wrapPathValue(em.diff.containerDir, w-4), "\n") {
		body = append(body, "    "+m.styles.meta.Render(ln))
	}
	body = append(body, "")
	for _, s := range em.published {
		body = append(body, "    "+m.styles.meta.Render(fmt.Sprintf("%s/  %s · %s · %s",
			s.short,
			humanize.Count(s.result.Files, "file", "files"),
			humanize.Count(s.result.Dirs, "dir", "dirs"),
			humanize.Bytes(s.result.Bytes))))
	}
	for _, short := range extractDiffSkippedSides(em) {
		body = append(body, "    "+m.styles.dim.Render(short+"  — nothing to extract on this side"))
	}
	if em.req.Privileged {
		body = append(body, "")
		body = append(body, m.extractDimNoteLines("extracted as root — snapshot file ownership preserved", w)...)
	}
	if unsafe > 0 {
		body = append(body, "")
		body = append(body, m.extractWarnLines(extractUnsafeSymlinkWarning(unsafe, em.cfg.UnsafeSymlinks), w)...)
	}
	return clipLines(body, w)
}

// extractDiffSkippedSides returns unpublished sides in display order after success.
func extractDiffSkippedSides(em extractModel) []string {
	ran := func(short string) bool {
		for _, s := range em.published {
			if s.short == short {
				return true
			}
		}
		return false
	}
	var out []string
	if !ran(em.diff.firstShort) {
		out = append(out, em.diff.firstShort)
	}
	if !ran(em.diff.secondShort) {
		out = append(out, em.diff.secondShort)
	}
	return out
}

// extractDiffPairValue labels the directional pair and any non-default filter.
func extractDiffPairValue(em extractModel) string {
	v := em.diff.firstShort + " → " + em.diff.secondShort
	if em.diff.filters != model.AllDiffKinds {
		v += " · filter: " + diffFilterLabel(em.diff.filters)
	}
	return v
}

// extractDiffChangesValue reports selected path counts, marking skipped sides.
func extractDiffChangesValue(em extractModel) string {
	return extractDiffSideCount(em.diff.firstShort, em.diff.firstCount) + " · " +
		extractDiffSideCount(em.diff.secondShort, em.diff.secondCount)
}

func extractDiffSideCount(short string, n int) string {
	if n == 0 {
		return short + ": nothing"
	}
	return short + ": " + humanize.Count(n, "changed path", "changed paths")
}

// extractDiffRunSideValue labels the active side's run-order position.
func extractDiffRunSideValue(em extractModel) string {
	total := len(em.published) + 1 + len(em.queue)
	return fmt.Sprintf("%s (%d of %d)", em.req.SnapshotShort, len(em.published)+1, total)
}

// extractDiffTerminalNote reports completed, failed, and unrun sides using short IDs only.
func extractDiffTerminalNote(em extractModel) string {
	parts := []string{em.req.SnapshotShort + " did not complete"}
	if len(em.published) > 0 {
		shorts := make([]string, len(em.published))
		for i, s := range em.published {
			shorts[i] = s.short
		}
		parts = append(parts, "already extracted: "+strings.Join(shorts, ", "))
	}
	if len(em.queue) > 0 {
		shorts := make([]string, len(em.queue))
		for i, q := range em.queue {
			shorts[i] = q.SnapshotShort
		}
		parts = append(parts, "not extracted: "+strings.Join(shorts, ", "))
	}
	return strings.Join(parts, " · ")
}

// extractTargetPath returns the pair container or plain mirrored target.
func extractTargetPath(em extractModel) string {
	if em.diff != nil {
		return em.diff.containerDir
	}
	return em.final
}

// extractTerminalBody offers cleanup when owned staging remains after failure.
func (m Model) extractTerminalBody(w int) string {
	em := m.extract
	var headline string
	switch em.state {
	case extractStateCanceled:
		headline = m.styles.bad.Render("! ") + extractCancelHeadline(em)
	default:
		headline = m.styles.errText.Render("× ") + extractErrorHeadline(em)
	}
	lines := []string{"  " + headline}
	if em.diff != nil {
		// Published diff sides remain in place after another side fails.
		lines = append(lines, "", "  "+m.styles.meta.Render(extractDiffTerminalNote(em)))
	}
	if em.stagingExists {
		// Resolve staging before retargeting so it cannot be orphaned.
		lines = append(lines,
			"",
			"  "+m.styles.meta.Render("Staging output (extract did not complete):"),
		)
		// Show the complete staging path for an informed cleanup choice.
		for ln := range strings.SplitSeq(wrapPathValue(em.result.StagingDir, w-4), "\n") {
			lines = append(lines, "    "+m.styles.meta.Render(ln))
		}
		lines = append(lines,
			"",
			"  "+m.styles.meta.Render("What should resticscope do with the staging dir?"),
			"    "+m.styles.dim.Render("k     keep as-is  (default)"),
			"    "+m.styles.dim.Render("d     delete the staging dir"),
		)
	} else if em.err != nil {
		// Only actionable occupied-path failures receive a retarget hint.
		if hint := extractRefusalHint(em); hint != "" {
			lines = append(lines, "", "  "+m.styles.dim.Render(hint))
		}
	}
	return clipLines(lines, w)
}

func extractCancelHeadline(em extractModel) string {
	p := em.progress
	parts := []string{"canceled"}
	if p.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("at %d / %s", p.FilesDone, humanize.Count(p.FilesTotal, "file", "files")))
	} else if p.FilesDone > 0 {
		parts = append(parts, "at "+humanize.Count(p.FilesDone, "file", "files"))
	}
	if p.BytesDone > 0 {
		parts = append(parts, humanize.Bytes(p.BytesDone)+" written")
	}
	return strings.Join(parts, " · ")
}

// extractErrorHeadline returns the first line of the app-layer error.
func extractErrorHeadline(em extractModel) string {
	if em.err == nil {
		return "extract failed"
	}
	return firstLine(em.err.Error())
}

// extractRefusalHint returns a path-free retarget hint only where its key is active.
func extractRefusalHint(em extractModel) string {
	if !isExtractRefusal(em.err) {
		return ""
	}
	if len(em.published) > 0 {
		// Do not split a partially published pair across roots.
		return "remove the existing output before retrying"
	}
	return "press t to choose another target, or remove the existing output"
}

// extractKeepDeleteBody renders while staging deletion is in flight.
func (m Model) extractKeepDeleteBody(w int) string {
	return clipLines([]string{
		"  " + m.styles.meta.Render("deleting staging directory…"),
	}, w)
}

// extractFilePickerBody clips the width-unaware picker and aligns its mode column.
func (m Model) extractFilePickerBody(w int) string {
	em := m.extract
	header := clip(m.styles.meta.Render("  "+em.filepicker.CurrentDirectory), w)
	body := clipLines(alignFilePickerModes(strings.Split(em.filepicker.View(), "\n"), em.pickerModeW), w)
	out := []string{header, "", body}
	if em.filepickerErr != "" {
		out = append(out, "", clip(m.styles.errText.Render("  "+em.filepickerErr), w))
	}
	return strings.Join(out, "\n")
}

// alignFilePickerModes pads variable-width modes using both visible rows and the
// directory-wide floor, keeping later columns stable while scrolling.
func alignFilePickerModes(lines []string, floor int) []string {
	type span struct{ at, n int }
	spans := make([]span, len(lines))
	maxw := floor
	for i, ln := range lines {
		at, n, ok := filePickerModeSpan(ln)
		if !ok {
			spans[i] = span{-1, 0}
			continue
		}
		spans[i] = span{at, n}
		maxw = max(maxw, n)
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		if sp := spans[i]; sp.at >= 0 && sp.n < maxw {
			ln = ln[:sp.at] + strings.Repeat(" ", maxw-sp.n) + ln[sp.at:]
		}
		out[i] = ln
	}
	return out
}

// filePickerModeSpan locates a styled mode token while skipping CSI sequences.
func filePickerModeSpan(line string) (at, n int, ok bool) {
	// FileMode.String's type, flag, permission, and absent-bit characters.
	const modeChars = "dalTLDpSugct?rwx-"
	visible := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			j := i + 1
			if j < len(line) && line[j] == '[' {
				for j++; j < len(line) && (line[j] < 0x40 || line[j] > 0x7e); j++ { //nolint:revive // intentional empty body: the loop's post-statement scans j past the CSI parameter bytes
				}
				if j < len(line) {
					j++
				}
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		switch {
		case visible < 2:
		case strings.ContainsRune(modeChars, r):
			n++
		default:
			// A real mode has at least ten cells and precedes the size-column gap.
			if n >= 10 && r == ' ' {
				return i, n, true
			}
			return 0, 0, false
		}
		visible++
		i += size
	}
	return 0, 0, false
}

// extractRow holds one labeled value, which may span lines.
type extractRow struct {
	label string
	value string
}

// renderExtractRows renders labeled fields; an empty row is a spacer.
func renderExtractRows(st styles, rows []extractRow, w int) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.label == "" && r.value == "" {
			out = append(out, "")
			continue
		}
		labelCell := st.extractLabel.Render(r.label)
		vlines := strings.Split(r.value, "\n")
		// Values use the terminal foreground shared by other main-content fields.
		first := clip("  "+labelCell+"  "+vlines[0], w)
		out = append(out, first)
		indent := strings.Repeat(" ", 2+labelWidth+2)
		for _, v := range vlines[1:] {
			out = append(out, clip(indent+v, w))
		}
	}
	return strings.Join(out, "\n")
}

// extractShortSnap prefers the precomputed short ID and otherwise truncates the full ID.
func extractShortSnap(req app.ExtractRequest) string {
	if req.SnapshotShort != "" {
		return req.SnapshotShort
	}
	return shortID(req.SnapshotID)
}

// extractSourceValue adds known size and a directory marker to the source path.
func extractSourceValue(em extractModel) string {
	v := em.req.Source
	if em.srcSize > 0 {
		v += " (" + humanize.Bytes(em.srcSize) + ")"
	}
	if extractSourceIsDir(em) {
		v = "▸ " + v
	}
	return v
}

// extractIsDir reports whether the published target is a directory.
func extractIsDir(em extractModel) bool {
	return em.diff != nil || em.req.Mode == app.ExtractDirectoryTree
}

// extractSourceIsDir uses originating row shape for typeless diff entries.
func extractSourceIsDir(em extractModel) bool {
	if em.diff != nil {
		return em.diff.sourceIsDir
	}
	return em.req.Mode == app.ExtractDirectoryTree
}

// extractSpaceShort warns only when size and free space are known; legacy
// snapshots use zero for unknown size.
func extractSpaceShort(em extractModel) bool {
	return em.targetFreeKnown && em.srcSize > 0 && em.srcSize > em.targetFree
}

// extractValueWidth matches renderExtractRows' gutter, label, and gap geometry.
func extractValueWidth(w int) int {
	return w - 2 - labelWidth - 2
}

// extractTargetValue wraps the full target and charges its directory marker to width.
func extractTargetValue(final string, avail int, dir bool) string {
	var prefix string
	if dir {
		prefix = "▸ "
		avail -= 2
	}
	return prefix + wrapPathValue(final, avail)
}

// wrapPathValue splits overlong paths on rune boundaries without truncation.
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
		end := min(i+avail, len(r))
		b.WriteString(string(r[i:end]))
	}
	return b.String()
}

// wrapWords wraps at spaces and rune-splits words that exceed the line budget.
func wrapWords(s string, avail int) string {
	if avail <= 0 || len([]rune(s)) <= avail {
		return s
	}
	var lines []string
	var cur string
	for word := range strings.FieldsSeq(s) {
		switch {
		case cur == "":
			cur = word
		case len([]rune(cur))+1+len([]rune(word)) <= avail:
			cur += " " + word
		default:
			lines = append(lines, wrapPathValue(cur, avail))
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, wrapPathValue(cur, avail))
	}
	return strings.Join(lines, "\n")
}

// collapsePath preserves the tail of an overlong running-state staging path.
func collapsePath(p string) string {
	const max = 60
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	// Preserve UTF-8 by slicing runes.
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
