package tui

import (
	"context"
	"path"
	"sort"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// The snapshot-diff controller uses generation checks and cancellation for its
// streamed tree navigator. clearSnapshotDiff removes path state on exit, and
// app.SnapshotDiff passes the password on fd 3 so paths and passwords never
// enter process arguments.

// diffProgressBuffer bounds non-blocking progress delivery so UI backpressure
// cannot stall restic output; later ticks replace any dropped count.
const diffProgressBuffer = 64

// toggleDetailMark toggles the selected snapshot in a two-slot FIFO, evicting
// the oldest mark when full.
func (m Model) toggleDetailMark() Model {
	snap := m.selectedSnapshot()
	if snap == nil {
		return m
	}
	id := snap.ID
	for i, s := range m.detailMarks {
		if s.ID == id {
			m.detailMarks = append(m.detailMarks[:i], m.detailMarks[i+1:]...)
			return m
		}
	}
	m.detailMarks = append(m.detailMarks, *snap)
	if len(m.detailMarks) > 2 {
		m.detailMarks = m.detailMarks[len(m.detailMarks)-2:]
	}
	return m
}

// clearDetailMarks clears marks only when leaving detail for the list, allowing
// them to survive browse and diff round trips.
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

// diffPair resolves a chronological older/newer pair so +/- describe changes
// in newer. False tells the caller to show a footer hint.
//
// Two marks form the pair; one mark pairs with the cursor. Zero marks or an
// identical cursor and sole mark produce no pair.
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

// startSnapshotDiff resets the view and starts a streamed diff. It supersedes
// prior work before clearing its cancellation function.
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

// dispatchSnapshotDiff starts a child cancellation scope plus stream and
// progress commands. Generation tags reject late messages from superseded runs.
func (m Model) dispatchSnapshotDiff(runOlder, runNewer model.Snapshot) (Model, tea.Cmd) {
	dctx, dcancel := context.WithCancel(m.ctx)
	m.diffCancel = dcancel
	m.diffLoadCount = 0
	m.diffErr = ""

	gen := m.diffGen
	progress := make(chan int, diffProgressBuffer)
	m.diffProgress = progress

	repo, olderID, newerID := m.diffRepo, runOlder.ID, runNewer.ID
	a := m.app
	streamCmd := func() tea.Msg {
		defer close(progress) // ends the paired waitForDiffProgress Cmd
		// Accumulate in the worker, then build the tree on the UI thread.
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

// waitForDiffProgress returns the next tick; Update rearms it until the stream
// closes the channel.
func waitForDiffProgress(gen int, progress <-chan int) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-progress
		if !ok {
			return nil
		}
		return snapshotDiffProgressMsg{gen: gen, seen: n}
	}
}

// applySnapshotDiffProgressMsg records a monotonic tick and rearms its matching
// generation. Stale generations are dropped without rearming.
func (m Model) applySnapshotDiffProgressMsg(msg snapshotDiffProgressMsg) (Model, tea.Cmd) {
	if msg.gen != m.diffGen {
		return m, nil
	}
	if msg.seen > m.diffLoadCount {
		m.diffLoadCount = msg.seen
	}
	return m, waitForDiffProgress(msg.gen, m.diffProgress)
}

// applySnapshotDiffMsg rejects stale results, preserves an existing tree on
// failure, returns to detail when nothing parsed, or installs a partial tree
// with a persistent warning.
func (m Model) applySnapshotDiffMsg(msg snapshotDiffMsg) Model {
	if msg.gen != m.diffGen {
		return m
	}
	m = m.cancelSnapshotDiff()
	if msg.err != nil {
		// A failed swap leaves the prior complete tree and a footer warning.
		if m.diffTree.Children != nil {
			m.statusMsg = "diff: " + firstLine(msg.err.Error())
			m.diffSelectPath = ""
			return m
		}
		if len(msg.entries) == 0 {
			m.statusMsg = "diff: " + firstLine(msg.err.Error())
			m.diffSelectPath = ""
			m = m.supersedeSnapshotDiff()
			m.view = detailView
			return m.clearSnapshotDiff()
		}
		// Keep partial-data warnings in diffErr; ordinary status messages disappear
		// on the next key and could make an incomplete tree look complete.
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

// handleSnapshotDiffKey routes diff actions and pauses navigation while loading
// rows that will soon be replaced.
func (m Model) handleSnapshotDiffKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Back) {
		if m.diffSearchJumped {
			return m.cancelDiffSearch(), nil
		}
		return m.snapshotDiffBack(), nil
	}

	m.statusMsg = ""
	if m.diffLoading() {
		return m, nil
	}

	// A multi-kind row remains visible while any of its modifier bits is enabled.
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

// swapSnapshotDiff reverses the pair while preserving filters and the requested
// landing path, falling back to its nearest existing parent.
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

// existingDiffDir returns requested or its nearest existing parent so a swapped
// result remains navigable.
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

// toggleDiffFilter flips one mask bit and rebuilds visible rows. Cached cursor
// positions survive and are clamped if filtering shrinks a directory.
func (m Model) toggleDiffFilter(k model.ModifierKind) Model {
	if m.diffSearchJumped {
		m = m.exitDiffSearch()
	}
	m.diffFilters ^= k
	// Restore the selected path if it survives the new filter.
	sel := ""
	if r := m.selectedDiffRow(); r != nil {
		sel = r.Path
	}
	return m.rebuildDiffRows(m.diffDir, sel)
}

// enterDiffDir enters a directory and caches the current cursor for back-nav.
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

// diffParentDir steps up one directory, restoring the cursor to the row it
// came from when the parent was last visited. Capped at the diff root.
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

// rebuildDiffRows filters and sorts a directory, then selects selectPath or its
// cached cursor position.
func (m Model) rebuildDiffRows(dir, selectPath string) Model {
	m.diffDir = dir
	kids := m.diffTree.Children[dir]
	filtered := make([]model.DiffRow, 0, len(kids))
	for _, r := range kids {
		if rowVisibleUnderFilter(r, m.diffFilters) {
			filtered = append(filtered, r)
		}
	}
	// Sort directories first, then names, preserving the builder's tie order.
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

// rowVisibleUnderFilter reports whether a row or directory aggregate has any
// enabled kind.
func rowVisibleUnderFilter(r model.DiffRow, filter model.ModifierKind) bool {
	if r.Kinds&filter != 0 {
		return true
	}
	if r.IsDir && r.Aggregate.EnabledKinds(filter) != 0 {
		return true
	}
	return false
}

// selectedDiffRow returns the cursor row or nil for an empty directory.
func (m Model) selectedDiffRow() *model.DiffRow {
	if m.diffCursor < 0 || m.diffCursor >= len(m.diffRows) {
		return nil
	}
	return &m.diffRows[m.diffCursor]
}

// openDiffExtract restores filtered changed paths from each side into separate
// snapshot roots in one diff container. Empty sides are skipped. Every request
// is a directory-tree restore with includes; IsDir affects only the review
// marker.
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
		// Non-pure directory changes do not become include paths.
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

// diffIncludesOverBudget mirrors PlanExtractPaths limits so oversized selections
// receive a useful status hint.
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

// snapshotDiffBack supersedes work before clearing state so racing messages are
// rejected. Detail marks survive the round trip.
func (m Model) snapshotDiffBack() Model {
	m.statusMsg = ""
	m = m.supersedeSnapshotDiff()
	m.view = detailView
	return m.clearSnapshotDiff()
}

// supersedeSnapshotDiff advances the generation and cancels in-flight work so
// late responses cannot restore stale state.
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

func (m Model) diffLoading() bool {
	return m.diffCancel != nil
}

// clearSnapshotDiff removes repository, snapshot, and path data when leaving
// the view. Detail marks remain until goBack reaches the list.
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
	m.diffLoadCount = 0
	m.diffCancel = nil
	m.diffProgress = nil
	return m
}

// diffVisible matches the view's row budget so page keys move exactly one
// window, with a floor of one.
func (m Model) diffVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + diffMetaRows + diffAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}
