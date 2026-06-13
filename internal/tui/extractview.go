package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// extractview.go renders extractView. Each state has its own body; the root
// Model's View() frames extractTitle + extractBody with the shared title row
// and pinned footer the same way browseTitle / browseBody are framed. Headings
// match the canonical mockups in 00-framework.md §14 byte-for-byte where
// layout allows.
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

// extractTitle picks the title label for the current sub-state. The per-state
// back/cancel affordances live in the footer key bar (extractModel.shortHelp),
// not up here.
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
		// mirroring browse's "browse: <repo> · <shortid>" header. A diff extract
		// names the directional pair instead of one side.
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

// extractBody returns the rendered body for the current sub-state. The root
// Model's View() frames this under the title row the same way browseBody is
// framed for browseView.
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
// serves both. (The §14 mockups predate this shape — the code is canonical.)
func (m Model) extractReviewBody(w int) string {
	em := m.extract
	// Repo + short snapshot id live in the header title; the source size is folded
	// into the Source row, so Type is gone, and the browse-style "▸ " dir marker
	// on Source / Target replaces the Output row. The key hints live in the footer
	// only.
	// Target wraps rather than elides (framework §8) so a long mirror path is
	// fully visible.
	rows := []extractRow{
		{label: "Source", value: extractSourceValue(em)},
	}
	if em.diff != nil {
		// The directional pair and the per-side changed-path tally — the diff
		// twin of the Contains row, sourced from the diff data itself (the
		// browse index's whole-subtree counts would be wrong here).
		rows = append(rows,
			extractRow{label: "Diff", value: extractDiffPairValue(em)},
			extractRow{label: "Changes", value: extractDiffChangesValue(em)},
		)
	}
	if em.srcCountsKnown {
		// Contained counts from the browse index — the "what will happen"
		// detail the Source row's recursive size alone doesn't carry for a
		// directory tree. Absent until the async lookup lands (and absent for
		// file sources, where the count is trivially one). Interior counts
		// only: the done screen's dir total also includes the restored source
		// dir itself, so it reads one higher by design.
		rows = append(rows, extractRow{label: "Contains", value: fmt.Sprintf("%s · %s",
			humanize.Count(em.srcFiles, "file", "files"),
			humanize.Count(em.srcDirs, "dir", "dirs"))})
	}
	rows = append(rows,
		extractRow{}, // spacer
		extractRow{label: "Target", value: extractTargetValue(extractTargetPath(em), extractValueWidth(w), extractIsDir(em))},
	)
	body := renderExtractRows(m.styles, rows, w)
	// Preflight occupancy note — the same FreshTargetCheck enter will enforce,
	// surfaced before the run so the collision isn't a surprise refusal screen.
	// State only: the retarget affordance lives in the footer key bar (t target).
	if em.targetBusy {
		body += "\n\n" + clip("  "+m.styles.errText.Render("target already exists — choose another target or remove the existing output"), w)
	}
	// Space preflight — advisory like the occupancy note: a run that outgrows
	// the filesystem still fails with restic's own error, but a hopeless
	// extraction is called out before the user commits to it.
	if extractSpaceShort(em) {
		body += "\n\n" + clip("  "+m.styles.errText.Render(fmt.Sprintf(
			"source may not fit the target filesystem — %s needed, %s free",
			humanize.Bytes(em.srcSize), humanize.Bytes(em.targetFree))), w)
	}
	// The privileged toggle's visible feedback (the Output row that used to carry
	// it is gone — the source/target dir markers convey the shape instead).
	if em.req.Privileged {
		body += "\n\n" + clip("  "+m.styles.dim.Render("as root — snapshot file ownership preserved"), w)
	}
	// One slot under the rows: while the sudo probe is in flight, a neutral hint
	// that the terminal may be handed over to sudo; otherwise the path-free red
	// notice (auth failed / privileged unavailable). Never both — every transition
	// that sets one clears the other.
	if em.sudoBusy {
		body += "\n\n" + clip("  "+m.styles.meta.Render("checking sudo access — the terminal may switch to a sudo password prompt"), w)
	} else if em.reviewNotice != "" {
		body += "\n\n" + clip("  "+m.styles.errText.Render(em.reviewNotice), w)
	}
	return body
}

// extractRunningBody renders the live-progress screen. A diff extract adds a
// Snapshot row naming the side currently restoring and its run-order position.
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
	barW := max(
		// leading indent + label + slack
		min(

			w-2-len([]rune(pctLabel))-2, 48), 16)
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
	fill := min(max(int(float64(width)*pct), 0), width)
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
	// No ETA segment: restic restore's JSON reports no seconds_remaining.
	return strings.Join(parts, " · ")
}

// extractSuccessBody renders the post-rename congratulation page. The s/q
// affordances live only in the footer key bar (shortHelp) — the body carries
// no key hints, so the two can never drift apart.
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
	// Count-only warning when the tree carried unsafe symlinks. No names — only the
	// count — so the line stays path-free even though FinalPath is shown above.
	if em.result.UnsafeSymlinks > 0 {
		body = append(body, "")
		body = append(body, m.extractWarnLines(extractUnsafeSymlinkWarning(em.result.UnsafeSymlinks, em.cfg.UnsafeSymlinks), w)...)
	}
	return clipLines(body, w)
}

// extractWarnLines renders a "! "-prefixed warning word-wrapped to the body
// width, so a narrow pane reflows the sentence instead of clipping it
// mid-word; continuation lines indent under the text column (past the marker).
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

// extractDimNoteLines renders an unmarked dim note word-wrapped to the body
// width, continuation lines sharing the two-cell gutter.
func (m Model) extractDimNoteLines(text string, w int) []string {
	lines := strings.Split(wrapWords(text, w-2), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, "  "+m.styles.dim.Render(ln))
	}
	return out
}

// extractUnsafeSymlinkWarning composes the success-screen warning for unsafe
// symlinks, phrased for the [extract] unsafe_symlinks policy that applied (the
// sub-model's own validated config — policy is config-only, never per-request).
// It carries only the count, never a path or a link name.
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

// extractDiffSuccessBody is the diff flavor of the done screen: a combined
// tally over the published sides, the pair container as the Target (the same
// dir shell-here lands in — one side's leaf would hide the other), and a
// per-side line under it, including a note for a side that had nothing to
// extract. Elapsed is the sides' sum: they run sequentially, so it is the
// operation's wall clock.
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

// extractDiffSkippedSides lists the pair sides that queued no request (nothing
// passed the filter on them), in display order. On the success screen every
// queued side is in published, so absence there is the skip signal.
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

// extractDiffPairValue renders the review's Diff row: the directional pair in
// display order plus, when not every change kind is enabled, the same filter
// mask vocabulary the diff view's summary line uses.
func extractDiffPairValue(em extractModel) string {
	v := em.diff.firstShort + " → " + em.diff.secondShort
	if em.diff.filters != model.AllDiffKinds {
		v += " · filter: " + diffFilterLabel(em.diff.filters)
	}
	return v
}

// extractDiffChangesValue is the review's per-side changed-path tally — what
// the diff data says will restore on each side. A side with no selection
// reads "nothing" and is skipped at run time.
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

// extractDiffRunSideValue labels the side currently restoring with its
// run-order position ("d27c2f56 (1 of 2)").
func extractDiffRunSideValue(em extractModel) string {
	total := len(em.published) + 1 + len(em.queue)
	return fmt.Sprintf("%s (%d of %d)", em.req.SnapshotShort, len(em.published)+1, total)
}

// extractDiffTerminalNote summarizes pair state on a failed or canceled diff
// side: which side stopped, what already published (and stays published), and
// what never ran. Shorts only — path-free like the rest of the terminal body.
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

// extractTargetPath is the Target row's path: the pair container for a diff
// extract (both snapshot roots land inside it), the exact mirror path
// otherwise.
func extractTargetPath(em extractModel) string {
	if em.diff != nil {
		return em.diff.containerDir
	}
	return em.final
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
		headline = m.styles.errText.Render("× ") + extractErrorHeadline(em)
	}
	lines := []string{"  " + headline}
	if em.diff != nil {
		// Pair context under the headline: a diff side that broke must not leave
		// the user guessing which sides landed (those stay published).
		lines = append(lines, "", "  "+m.styles.meta.Render(extractDiffTerminalNote(em)))
	}
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
		// No staging to resolve: surface the actionable refusal hint (only for an
		// occupied-target/staging refusal — handleTerminalKey honors `t` here).
		// The back affordance lives in the footer key bar, not the body.
		if hint := extractRefusalHint(em); hint != "" {
			lines = append(lines, "", "  "+m.styles.dim.Render(hint))
		}
	}
	return clipLines(lines, w)
}

// extractCancelHeadline composes the "! canceled at … bytes written" line.
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
	if !isExtractRefusal(em.err) {
		return ""
	}
	if len(em.published) > 0 {
		// A diff side already landed under the current root; retargeting the rest
		// would split the pair container, so handleTerminalKey withholds t here.
		return "remove the existing output before retrying"
	}
	return "press t to choose another target, or remove the existing output"
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
// would otherwise overflow past the layout boundary. Lines also pass through
// alignFilePickerModes, fixing the picker's wobbling size/name columns.
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

// alignFilePickerModes left-pads the picker's permission column to a uniform
// width so the size and name columns line up. Go's FileMode.String() emits a
// variable-length type prefix — "-rw-r--r--" is 10 cells but a sticky dir is
// "dtrwxrwxrwx" (11) — and bubbles' filepicker (v2.1.0) writes it unpadded in
// both its cursor-row and plain-row branches. The cursor row never goes
// through Styles.Permission, so a Width on that style cannot fix it; instead
// the rendered lines are normalized here, padding each mode to the widest of
// the on-screen modes and floor — the directory-wide max (pickerModeW), so the
// columns hold still as wide-mode rows scroll out of the viewport. Inserted
// spaces inherit whatever SGR attributes are open at that point, which is
// harmless: the picker only sets foreground and bold.
func alignFilePickerModes(lines []string, floor int) []string {
	type span struct{ at, n int } // byte offset just past the mode token, and its cell count
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

// filePickerModeSpan locates the mode token of one rendered picker row: the
// first visible token after the one-cell cursor column and its following gap.
// It walks the raw string skipping CSI escape sequences, so the returned byte
// offset can be used to splice padding into the styled line. ok is false for
// rows that do not carry a mode at that position (filler lines, the
// empty-directory notice), which are left untouched.
func filePickerModeSpan(line string) (at, n int, ok bool) {
	// Every character Go's FileMode.String() can produce: the type/flag prefix
	// alphabet, '-' for "no bits", and the rwx permission triplets.
	const modeChars = "dalTLDpSugct?rwx-"
	visible := 0 // visible cell index; everything up to the mode is one cell per rune
	for i := 0; i < len(line); {
		if line[i] == 0x1b { // skip a CSI sequence: ESC '[' params final-byte
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
		case visible < 2: // the cursor cell and the gap before the mode
		case strings.ContainsRune(modeChars, r):
			n++
		default:
			// First rune past the mode: a real mode is ≥10 cells ("-rw-r--r--")
			// and is always followed by the size column's leading space.
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
// only when known since the request doesn't carry per-entry counts. A directory
// source gets the same "▸ " marker the browse list uses for dir rows — that
// marker is the only file-vs-directory signal on the screen now that the Output
// row is gone.
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

// extractIsDir reports whether the published target node is a directory — the
// condition for the browse-style "▸ " dir marker on the review's Target row.
// Tree mode publishes a directory in every case but isn't consulted for a
// diff extract, whose Target row is the pair container (always a directory).
func extractIsDir(em extractModel) bool {
	return em.diff != nil || em.req.Mode == app.ExtractDirectoryTree
}

// extractSourceIsDir reports whether the source node is a directory — the
// marker condition for the review's Source row. A plain extract derives it
// from the mode; a diff extract is always tree-mode regardless of leaf type
// (diff entries attest no node type), so its meta carries the originating
// row's shape instead.
func extractSourceIsDir(em extractModel) bool {
	if em.diff != nil {
		return em.diff.sourceIsDir
	}
	return em.req.Mode == app.ExtractDirectoryTree
}

// extractSpaceShort reports whether the review's space preflight should warn:
// both sides must be known — srcSize 0 means "size unknown" (a pre-0.17
// snapshot without a summary) and never warns, and an unanswered probe stays
// silent rather than guessing.
func extractSpaceShort(em extractModel) bool {
	return em.targetFreeKnown && em.srcSize > 0 && em.srcSize > em.targetFree
}

// extractValueWidth is the cell budget for a labeled row's value: the full width
// less the gutter (2), the fixed label column (labelWidth), and the label/value
// gap (2). It matches renderExtractRows's own layout so a pre-wrapped value's
// lines each fit and clip is left a no-op.
func extractValueWidth(w int) int {
	return w - 2 - labelWidth - 2
}

// extractTargetValue renders the final dir for the review screen: one line when
// it fits in avail, otherwise wrapped across continuation lines so a long
// mirror path is fully visible rather than elided (framework §8) — the same
// treatment the staging path gets on the terminal screens. A directory
// extraction gets the browse-style "▸ " marker, mirroring the Source row; its
// 2 cells count against the fit budget.
func extractTargetValue(final string, avail int, dir bool) string {
	var prefix string
	if dir {
		prefix = "▸ "
		avail -= 2
	}
	return prefix + wrapPathValue(final, avail)
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
		end := min(i+avail, len(r))
		b.WriteString(string(r[i:end]))
	}
	return b.String()
}

// wrapWords word-wraps prose into "\n"-joined runs of at most avail cells,
// breaking at spaces so a sentence reflows on a narrow pane instead of being
// truncated mid-word. A single word longer than avail falls back to
// wrapPathValue's hard rune split, so no resulting line can exceed the budget.
// Same contract as wrapPathValue otherwise: text that already fits (or a
// non-positive avail) is returned unchanged.
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
