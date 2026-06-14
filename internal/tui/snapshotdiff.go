package tui

import (
	"context"
	"path"
	"sort"

	"resticscope/internal/app"
	"resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// snapshotdiff.go is the controller for snapshotDiffView: the tree-style diff
// navigator launched from the detail view by marking two snapshots and pressing
// `d`. It mirrors findversions.go's generation/cancel discipline and browse.go's
// progress-channel pattern. The on-screen rows hold paths only for the lifetime
// of the model — clearSnapshotDiff zeros every diff* field on leaving the view
// (non-negotiable #1: no filenames linger). The stream itself goes through
// app.SnapshotDiff which delegates to Restic.StreamDiff with the password on
// fd 3, so no path or password ever touches a process arg.

// diffProgressBuffer bounds the diff progress channel. Sends are non-blocking,
// so the restic stdout consumer never stalls behind a full UI channel: a
// dropped tick is harmless because a later tick (or the final count) carries a
// fresher number. Mirrors browseProgressBuffer.
const diffProgressBuffer = 64

// toggleDetailMark applies the 2-slot FIFO toggle for the snapshot under the
// detail-view cursor: re-marking an already-marked row clears it; otherwise the
// row joins the FIFO, evicting the oldest mark when the slot count is full.
func (m Model) toggleDetailMark() Model {
	snap := m.selectedSnapshot()
	if snap == nil {
		return m
	}
	id := snap.ID
	for i, s := range m.detailMarks {
		if s.ID == id {
			// Re-mark clears in place; the surviving mark (if any) keeps its slot.
			m.detailMarks = append(m.detailMarks[:i], m.detailMarks[i+1:]...)
			return m
		}
	}
	m.detailMarks = append(m.detailMarks, *snap)
	if len(m.detailMarks) > 2 {
		// FIFO eviction: drop the oldest entry once a third mark arrives.
		m.detailMarks = m.detailMarks[len(m.detailMarks)-2:]
	}
	return m
}

// clearDetailMarks is the single chokepoint that zeroes the FIFO. Called from
// goBack's detailView arm when leaving the detail context for the list, so
// marks survive a round-trip into snapshotDiffView or browseView (sub-contexts
// of detail) and only clear on the way home.
func (m Model) clearDetailMarks() Model {
	m.detailMarks = nil
	return m
}

// isMarked reports whether the snapshot with the given ID is in the FIFO.
func (m Model) isMarked(id string) bool {
	for _, s := range m.detailMarks {
		if s.ID == id {
			return true
		}
	}
	return false
}

// diffPair resolves the (older, newer) pair to diff from the current detail
// state. ok=false signals the caller to show a footer hint instead of opening
// the view. The pair is always sorted chronologically (older.Time <= newer.Time)
// so the renderer can show an unambiguous arrow and `+`/`-` mean "added/removed
// in the newer".
//
//   - 2 marks → diff those two (irrespective of cursor)
//   - 1 mark  → diff that mark against the cursor row (anchor + cursor)
//   - 0 marks → ok=false, footer hint
//   - cursor identical to the only mark → ok=false, footer hint
func (m Model) diffPair() (older, newer model.Snapshot, ok bool) {
	marks := normalizedDetailMarks(m.detailMarks, m.snapDisplay())
	switch len(marks) {
	case 2:
		older, newer = marks[0], marks[1]
	case 1:
		cur := m.selectedSnapshot()
		if cur == nil || cur.ID == marks[0].ID {
			return model.Snapshot{}, model.Snapshot{}, false
		}
		older, newer = marks[0], *cur
	default:
		return model.Snapshot{}, model.Snapshot{}, false
	}
	if newer.Time.Before(older.Time) {
		older, newer = newer, older
	}
	return older, newer, true
}

// startSnapshotDiff enters snapshotDiffView for the (older, newer) pair. It
// pins the pair on the model, resets the view to the diff root with all
// filters on, and kicks off the streamed restic diff. clearSnapshotDiff is the
// single source of truth for the zero state; supersede first so a prior
// in-flight diff is cancelled before clear drops its cancel function.
func (m Model) startSnapshotDiff(repo string, older, newer model.Snapshot) (Model, tea.Cmd) {
	m = m.supersedeSnapshotDiff()
	m = m.clearSnapshotDiff()
	m.diffRepo = repo
	m.diffOlder = older
	m.diffNewer = newer
	m.diffDir = model.DiffRoot
	m.diffFilters = model.AllDiffKinds
	m.diffCache = make(map[string]int)
	m.statusMsg = ""
	m.view = snapshotDiffView
	return m.dispatchSnapshotDiff(m.diffOlder, m.diffNewer)
}

// dispatchSnapshotDiff opens a fresh cancel scope (a child of m.ctx so a
// program-level quit still cascades, but back/esc can cancel just the diff),
// kicks off the stream in a Cmd, and arms a paired Cmd that waits on the
// progress channel. Both Cmds carry the generation so a late tick or terminal
// message from a superseded run is dropped.
func (m Model) dispatchSnapshotDiff(runOlder, runNewer model.Snapshot) (Model, tea.Cmd) {
	dctx, dcancel := context.WithCancel(m.ctx)
	m.diffCancel = dcancel
	m.isDiffLoading = true
	m.diffLoadCount = 0
	m.diffErr = ""

	gen := m.diffGen
	progress := make(chan int, diffProgressBuffer)
	m.diffProgress = progress

	repo, olderID, newerID := m.diffRepo, runOlder.ID, runNewer.ID
	a := m.app
	streamCmd := func() tea.Msg {
		defer close(progress) // ends the paired waitForDiffProgress Cmd
		// entries live in this closure for the duration of the stream; the
		// terminal msg hands them to applySnapshotDiffMsg which calls
		// BuildDiffTree on the UI thread.
		var entries []model.DiffEntry
		res, err := a.SnapshotDiff(dctx, repo, olderID, newerID,
			func(e model.DiffEntry) error {
				entries = append(entries, e)
				return nil
			},
			func(seen int) {
				select {
				case progress <- seen:
				default:
				}
			})
		return snapshotDiffMsg{gen: gen, older: runOlder, newer: runNewer, result: res, entries: entries, err: err}
	}
	return m, tea.Batch(streamCmd, waitForDiffProgress(gen, progress))
}

// waitForDiffProgress blocks on the progress channel and turns the next tick
// into a snapshotDiffProgressMsg, re-arming itself in Update so the count
// climbs live. A closed channel (the stream Cmd finished) returns a nil msg,
// which ends the loop.
func waitForDiffProgress(gen int, progress <-chan int) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-progress
		if !ok {
			return nil
		}
		return snapshotDiffProgressMsg{gen: gen, seen: n}
	}
}

// applySnapshotDiffProgressMsg records a progress tick and re-arms the wait
// command. A tick whose generation no longer matches is from a superseded run
// and is dropped without re-arming (the superseding run owns its own channel).
// The count is clamped monotonic so out-of-order ticks never make it jump
// backwards.
func (m Model) applySnapshotDiffProgressMsg(msg snapshotDiffProgressMsg) (Model, tea.Cmd) {
	if msg.gen != m.diffGen {
		return m, nil
	}
	if msg.seen > m.diffLoadCount {
		m.diffLoadCount = msg.seen
	}
	return m, waitForDiffProgress(msg.gen, m.diffProgress)
}

// applySnapshotDiffMsg installs the terminal result of a streamed diff. A
// message whose generation no longer matches is from a superseded or
// cancelled request and is dropped. On error with a previous tree on screen
// (swap path) the previous tree is preserved and the error is surfaced in
// statusMsg (transient, fine here — the user already had a complete tree).
// On a first-time error with no parsed entries the view falls back to detail
// so the user is not stranded. On a first-time error with partial entries
// the partial tree is installed and diffErr carries a sticky "partial: …"
// warning that rides on the summary line for the lifetime of the tree (and
// is prefixed onto the search summary while search is open) — dropping
// thousands of good entries because one late line was malformed would be
// worse than showing them with a warning, but a transient statusMsg would
// let the next navigation/filter/search key dismiss the warning and leave
// an incomplete tree indistinguishable from a complete one.
// On success BuildDiffTree turns the flat entry stream into the virtual
// per-dir listing and the cursor lands on the requested diffDir (root for a
// fresh open, the current dir for a swap).
func (m Model) applySnapshotDiffMsg(msg snapshotDiffMsg) Model {
	if msg.gen != m.diffGen {
		return m
	}
	m.isDiffLoading = false
	m = m.cancelSnapshotDiff()
	if msg.err != nil {
		// Swap error: keep the previous tree on screen. statusMsg's footer
		// notice is fine here because the user already had a complete tree;
		// the warning is about a *failed* swap, not the data on screen.
		if m.diffTree.Children != nil {
			m.statusMsg = "diff: " + firstLine(msg.err.Error())
			m.diffSelectPath = ""
			return m
		}
		// First-time error with no entries: nothing to show, bail to detail.
		if len(msg.entries) == 0 {
			m.statusMsg = "diff: " + firstLine(msg.err.Error())
			m.diffSelectPath = ""
			m = m.supersedeSnapshotDiff()
			m.view = detailView
			return m.clearSnapshotDiff()
		}
		// First-time error with partial entries: install what we got and
		// pin the warning to diffErr so it rides on the summary line for
		// the lifetime of this tree. Putting it in statusMsg would let the
		// next j/filter/search key silently dismiss it (handleSnapshotDiffKey
		// clears statusMsg), leaving an incomplete tree indistinguishable
		// from a complete one.
		m.diffErr = "partial: " + firstLine(msg.err.Error())
	} else {
		m.diffErr = ""
	}
	m.diffOlder = msg.older
	m.diffNewer = msg.newer
	m.diffEntries = msg.entries
	m.diffTree = model.BuildDiffTree(msg.entries)
	m.diffStats = m.diffTree.Aggregate[model.DiffRoot]
	m.diffParseErrs = msg.result.ParseErrors
	selectPath := m.diffSelectPath
	m.diffSelectPath = ""
	return m.rebuildDiffRows(existingDiffDir(m.diffTree, m.diffDir), selectPath)
}

// handleSnapshotDiffKey routes keys in the snapshot-diff view. Back (esc or
// the contextual q routed by handleKey's Quit branch) returns to detail with
// the in-flight stream cancelled. Swap (`x`) flips the directional snapshot
// pair and reruns restic diff. Filter toggles (+, -, M, U, T, b) flip the
// corresponding bit in diffFilters and rebuild the visible rows. Extract (e)
// opens the modal for the selected row's changed paths. Navigation is
// paused while a stream is loading because the rows are about to be replaced.
func (m Model) handleSnapshotDiffKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Back) {
		if m.diffSearchJumped {
			return m.cancelDiffSearch(), nil
		}
		return m.snapshotDiffBack(), nil
	}

	m.statusMsg = ""
	if m.isDiffLoading {
		return m, nil
	}

	// Filter toggles. Each binding maps one restic modifier character to its
	// bit in the filter mask; the row predicate `row.Kinds & filter != 0`
	// keeps multi-kind rows (e.g. `MU`) visible whenever any of their bits is
	// enabled, so toggling `M` off does NOT hide a `MU` row.
	switch {
	case key.Matches(msg, m.keys.DiffSwap):
		return m.swapSnapshotDiff()
	case key.Matches(msg, m.keys.Search):
		return m.openDiffSearch(), nil
	case key.Matches(msg, m.keys.DiffFilterAdded):
		return m.toggleDiffFilter(model.KindAdded), nil
	case key.Matches(msg, m.keys.DiffFilterRemoved):
		return m.toggleDiffFilter(model.KindRemoved), nil
	case key.Matches(msg, m.keys.DiffFilterModified):
		return m.toggleDiffFilter(model.KindModified), nil
	case key.Matches(msg, m.keys.DiffFilterMetadata):
		return m.toggleDiffFilter(model.KindMetadata), nil
	case key.Matches(msg, m.keys.DiffFilterTypeChanged):
		return m.toggleDiffFilter(model.KindTypeChanged), nil
	case key.Matches(msg, m.keys.DiffFilterBitrot):
		return m.toggleDiffFilter(model.KindBitrot), nil
	case key.Matches(msg, m.keys.Extract):
		return m.openDiffExtract()
	}

	switch {
	case key.Matches(msg, m.keys.Up):
		if m.diffCursor > 0 {
			m.diffCursor--
		}
	case key.Matches(msg, m.keys.Down):
		if m.diffCursor < len(m.diffRows)-1 {
			m.diffCursor++
		}
	case key.Matches(msg, m.keys.PageUp):
		m.diffCursor = clampCursor(m.diffCursor-m.diffVisible(), len(m.diffRows))
	case key.Matches(msg, m.keys.PageDown):
		m.diffCursor = clampCursor(m.diffCursor+m.diffVisible(), len(m.diffRows))
	case key.Matches(msg, m.keys.Enter), key.Matches(msg, m.keys.Open):
		m = m.enterDiffDir()
	case key.Matches(msg, m.keys.Parent):
		m = m.diffParentDir()
	}
	return m, nil
}

// swapSnapshotDiff flips the already-open directional pair and reruns the diff
// with the same filter mask. The current diffDir is kept as the requested
// landing path for the terminal result; applySnapshotDiffMsg falls back to the
// nearest existing parent if the path is absent from the swapped result.
func (m Model) swapSnapshotDiff() (Model, tea.Cmd) {
	if m.diffOlder.ID == "" || m.diffNewer.ID == "" {
		return m, nil
	}
	dir := m.diffDir
	if dir == "" {
		dir = model.DiffRoot
	}
	selectPath := ""
	if r := m.selectedDiffRow(); r != nil {
		selectPath = r.Path
	}
	m = m.exitDiffSearch()
	m = m.supersedeSnapshotDiff()
	m.diffDir = dir
	m.diffSelectPath = selectPath
	m.statusMsg = ""
	return m.dispatchSnapshotDiff(m.diffNewer, m.diffOlder)
}

// existingDiffDir returns requested when it exists in tree, otherwise the
// nearest existing parent. This keeps directional swap in the same directory
// for the normal case while still giving the view a navigable path if filters
// or restic output shape remove the exact directory.
func existingDiffDir(tree model.DiffTree, requested string) string {
	if requested == "" {
		requested = model.DiffRoot
	}
	for dir := requested; dir != ""; dir = model.DiffParentOf(dir) {
		if diffTreeHasDir(tree, dir) {
			return dir
		}
	}
	return model.DiffRoot
}

func diffTreeHasDir(tree model.DiffTree, dir string) bool {
	if dir == "" || dir == model.DiffRoot {
		return true
	}
	parent := model.DiffParentOf(dir)
	for _, r := range tree.Children[parent] {
		if r.Path == dir && r.IsDir {
			return true
		}
	}
	return false
}

// toggleDiffFilter flips the supplied bit in the filter mask and rebuilds the
// visible rows for the current dir, clamping the cursor. Bookmarked dir
// cursors in diffCache are kept — they index into the next filtered listing
// when the user navigates back, and clampCursor handles the case where the
// dir's row count shrank below the saved index.
func (m Model) toggleDiffFilter(k model.ModifierKind) Model {
	if m.diffSearchJumped {
		m = m.exitDiffSearch()
	}
	m.diffFilters ^= k
	// Remember where we were so the rebuild can restore the cursor onto the
	// same row by path if it survives the new filter.
	sel := ""
	if r := m.selectedDiffRow(); r != nil {
		sel = r.Path
	}
	return m.rebuildDiffRows(m.diffDir, sel)
}

// enterDiffDir steps into the selected dir row. Files have no action (the
// per-file content drill-in is a deliberately separate follow-up surface).
// The current cursor index is remembered in diffCache against the *current*
// dir, keyed by name, so a back-nav lands on the row we came from.
func (m Model) enterDiffDir() Model {
	r := m.selectedDiffRow()
	if r == nil || !r.IsDir {
		return m
	}
	if m.diffCache == nil {
		m.diffCache = make(map[string]int)
	}
	m.diffCache[m.diffDir] = m.diffCursor
	return m.rebuildDiffRows(r.Path, "")
}

// diffParentDir steps up one directory, restoring the cursor to the row we
// came from when the parent's listing was last visited. Capped at the diff
// root.
func (m Model) diffParentDir() Model {
	if m.diffDir == model.DiffRoot || m.diffDir == "" {
		return m
	}
	parent := model.DiffParentOf(m.diffDir)
	if parent == "" {
		parent = model.DiffRoot
	}
	from := m.diffDir
	return m.rebuildDiffRows(parent, from)
}

// rebuildDiffRows replaces diffRows with the (filtered, sorted) children of
// dir and positions the cursor. selectPath, when non-empty, asks the cursor
// to land on that child by full path; otherwise the cursor lands on the
// remembered index from diffCache, or 0.
func (m Model) rebuildDiffRows(dir, selectPath string) Model {
	m.diffDir = dir
	kids := m.diffTree.Children[dir]
	filtered := make([]model.DiffRow, 0, len(kids))
	for _, r := range kids {
		if rowVisibleUnderFilter(r, m.diffFilters) {
			filtered = append(filtered, r)
		}
	}
	// Sort: dirs first, then by name. Stable so the tree builder's pre-sort is
	// the tie-breaker.
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].IsDir != filtered[j].IsDir {
			return filtered[i].IsDir
		}
		return filtered[i].Name < filtered[j].Name
	})
	m.diffRows = filtered
	cursor := 0
	if selectPath != "" {
		for i, r := range filtered {
			if r.Path == selectPath {
				cursor = i
				break
			}
		}
	} else if remembered, ok := m.diffCache[dir]; ok {
		cursor = clampCursor(remembered, len(filtered))
	}
	m.diffCursor = clampCursor(cursor, len(filtered))
	return m
}

// rowVisibleUnderFilter mirrors the documented filter predicate: a row is
// visible when any of its set kinds is enabled. Dir rows fall back to their
// aggregate (so a dir whose entire subtree is filtered out drops).
func rowVisibleUnderFilter(r model.DiffRow, filter model.ModifierKind) bool {
	if r.Kinds&filter != 0 {
		return true
	}
	if r.IsDir && r.Aggregate.EnabledKinds(filter) != 0 {
		return true
	}
	return false
}

// selectedDiffRow returns the row under the cursor or nil when the cursor is
// out of range (empty dir).
func (m Model) selectedDiffRow() *model.DiffRow {
	if m.diffCursor < 0 || m.diffCursor >= len(m.diffRows) {
		return nil
	}
	return &m.diffRows[m.diffCursor]
}

// openDiffExtract launches the extract modal for the selected diff row: a
// changed-paths-only restore of the row's subtree from each side of the pair
// into one diff container — two snapshot-id roots, ready for a local diff -r.
// The include sets honor the live filter mask, so what extracts is exactly
// what the view shows; a side with nothing selected is skipped outright. The
// row's IsDir has no bearing on the restic shape (diff entries attest no node
// type, so every diff request is a directory-tree restore with includes); it
// only drives the review screen's dir marker.
func (m Model) openDiffExtract() (Model, tea.Cmd) {
	r := m.selectedDiffRow()
	if r == nil {
		return m, nil
	}
	if len(m.diffOlder.ID) < 8 || len(m.diffNewer.ID) < 8 {
		m.statusMsg = "extract: snapshot ids unavailable"
		return m, nil
	}
	set := model.DiffExtractIncludes(m.diffEntries, r.Path, m.diffFilters)
	if len(set.First) == 0 && len(set.Second) == 0 {
		// Either nothing under the row passes the filter, or the only passing
		// changes are non-pure directory entries (which never become includes).
		m.statusMsg = "extract: no extractable changes under the active filter"
		return m, nil
	}
	if diffIncludesOverBudget(set.First) || diffIncludesOverBudget(set.Second) {
		m.statusMsg = "extract: too many changed paths — narrow the filter or pick a deeper directory"
		return m, nil
	}
	name, err := app.SanitizeExtractSlug(path.Base(r.Path))
	if err != nil {
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m, nil
	}
	firstShort, secondShort := m.diffOlder.ID[:8], m.diffNewer.ID[:8]
	container := "diff-" + firstShort + "-" + secondShort
	mk := func(snap model.Snapshot, incs []string) app.ExtractRequest {
		return app.ExtractRequest{
			Repo:          m.diffRepo,
			SnapshotID:    snap.ID,
			SnapshotShort: snap.ID[:8],
			Source:        r.Path,
			SourceName:    name,
			Mode:          app.ExtractDirectoryTree,
			DiffContainer: container,
			IncludePaths:  incs,
		}
	}
	var reqs []app.ExtractRequest
	if len(set.First) > 0 {
		reqs = append(reqs, m.seedTargetMemo(mk(m.diffOlder, set.First)))
	}
	if len(set.Second) > 0 {
		reqs = append(reqs, m.seedTargetMemo(mk(m.diffNewer, set.Second)))
	}
	sub, err := newExtractDiffModel(m.app, m.ctx, reqs, extractDiffMeta{
		firstShort:  firstShort,
		secondShort: secondShort,
		filters:     m.diffFilters,
		sourceIsDir: r.IsDir,
		firstCount:  set.FirstCount,
		secondCount: set.SecondCount,
	})
	if err != nil {
		// PlanExtractPaths-style errors are path-free by contract.
		m.statusMsg = "extract: " + firstLine(err.Error())
		return m, nil
	}
	m.statusMsg = ""
	m.extract = sub
	m.extractReturn = snapshotDiffView
	m.view = extractView
	return m, nil
}

// diffIncludesOverBudget pre-checks one side's include list against the same
// caps PlanExtractPaths enforces, so an oversized selection is refused with a
// friendly status-line hint instead of a request-validation error.
func diffIncludesOverBudget(incs []string) bool {
	if len(incs) > app.MaxDiffExtractIncludes {
		return true
	}
	total := 0
	for _, p := range incs {
		total += len(p)
	}
	return total > app.MaxDiffExtractIncludeBytes
}

// snapshotDiffBack leaves the diff view, shared by q (routed in handleKey's
// Quit branch) and esc. Order matters: advance the generation first so any
// racing snapshotDiffMsg is dropped on arrival, then cancel the in-flight
// stream so it doesn't outlive the user's exit, only then switch the view and
// clear the diff* fields. Marks are *not* cleared here: they belong to the
// detail context and survive this round-trip.
func (m Model) snapshotDiffBack() Model {
	m.statusMsg = ""
	m = m.supersedeSnapshotDiff()
	m.view = detailView
	return m.clearSnapshotDiff()
}

// supersedeSnapshotDiff advances the diff generation and cancels any in-flight
// diff. Mirrors supersedeBrowse / supersedeFind — used by every entry point
// that starts or ends a diff so a late response cannot resurrect stale state.
func (m Model) supersedeSnapshotDiff() Model {
	m.diffGen++
	return m.cancelSnapshotDiff()
}

func (m Model) cancelSnapshotDiff() Model {
	if m.diffCancel != nil {
		m.diffCancel()
		m.diffCancel = nil
	}
	return m
}

// clearSnapshotDiff zeroes every diff* field. Called on every exit from the
// view so no repo, snapshot, path, or entry data lingers in the model past the
// user's leave (non-negotiable #1: paths do not persist). Marks are deliberately
// untouched — they belong to the detail context, cleared only by goBack on the
// way to the list.
func (m Model) clearSnapshotDiff() Model {
	m.diffRepo = ""
	m.diffOlder = model.Snapshot{}
	m.diffNewer = model.Snapshot{}
	m.diffEntries = nil
	m.diffTree = model.DiffTree{}
	m.diffDir = ""
	m.diffRows = nil
	m.diffCursor = 0
	m.diffCache = nil
	m.diffSelectPath = ""
	m = m.exitDiffSearch()
	m.diffFilters = 0
	m.diffStats = model.DiffStats{}
	m.diffErr = ""
	m.diffParseErrs = 0
	m.isDiffLoading = false
	m.diffLoadCount = 0
	m.diffCancel = nil
	m.diffProgress = nil
	return m
}

// diffVisible is the number of diff rows the table shows at once. Floored at 1.
// The actual overhead is computed by the view; this controller-side helper
// matches the view's calculation so PgUp/PgDn jump exactly one window.
func (m Model) diffVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + diffMetaRows + diffAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}
