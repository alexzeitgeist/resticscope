package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// info.go renders the full-screen snapshot-info modal (key `i` from detail): a
// labeled dump of as much of the selected snapshot record as restic's
// `snapshots --json` makes available. It mirrors help.go's modal pattern —
// title row, body, closed via `i`, `q`, or `esc` — and reuses the same heading
// / label / meta styles. The bound is intentionally split off m.field's
// labelWidth: rows like "Files unmodified" or "Data added packed" exceed the
// detail panel's 11-cell label column, so the renderer computes its own column
// width.

// infoLabelMin is the floor for the info modal's label column. Real labels go
// up to ~17 cells; the floor keeps a degenerate empty group from collapsing the
// value column against the gutter.
const infoLabelMin = 12

// infoRow is one label/value pair. multi true means each value renders on its
// own line, with continuation lines indented to the value column.
type infoRow struct {
	label  string
	values []string
	multi  bool
}

// infoSection groups rows under a heading. An empty rows slice is silently
// skipped by the renderer so missing data (e.g. pre-0.17 summary) leaves no
// visible gap.
type infoSection struct {
	title string
	rows  []infoRow
}

// infoTitle names the info context: the detail repo and the inspected
// snapshot's short id. detailName is used directly (it is what detailRow()
// searches by and survives a refresh that drops the row); the snapshot part is
// omitted when no snapshot is selected, and the bare "info" fallback covers an
// empty detailName so the title can never be blank.
func (m Model) infoTitle() string {
	if m.detailName == "" {
		return m.styles.title.Render("info")
	}
	label := "info: " + m.detailName
	if s := m.selectedSnapshot(); s != nil {
		id := s.ShortID
		if id == "" {
			id = shortID(s.ID)
		}
		if id != "" {
			label += " · " + id
		}
	}
	return m.styles.title.Render(label)
}

func (m Model) infoBody() string {
	s := m.selectedSnapshot()
	if s == nil {
		w, _ := m.effSize()
		return clip(m.styles.meta.Render("no snapshot selected"), w)
	}
	w, _ := m.effSize()
	lines := m.infoBodyLines(*s, w)
	visible := m.modalVisible()
	if len(lines) <= visible {
		return strings.Join(lines, "\n")
	}
	// Content overflows: reserve the last visible row for a scroll hint and
	// window the rest around m.infoScroll. clampModalScroll guarantees the same
	// bounds the key handler enforces, so the model state and what is on screen
	// can never disagree. When visible==1 there is no room for both body and
	// hint — drop the hint so the modal never spills into an adjacent pane.
	bodyRows := visible - 1
	showHint := bodyRows >= 1
	if !showHint {
		bodyRows = visible
	}
	start := clampModalScroll(m.infoScroll, len(lines), bodyRows)
	end := min(start+bodyRows, len(lines))
	out := make([]string, 0, end-start+1)
	out = append(out, lines[start:end]...)
	if showHint {
		hint := fmt.Sprintf("  showing lines %d–%d of %d", start+1, end, len(lines))
		out = append(out, clip(m.styles.meta.Render(hint), w))
	}
	return strings.Join(out, "\n")
}

// infoBodyLines builds the modal body as a flat line list so the renderer can
// window it. Sections are separated by a single blank line; an empty section is
// silently skipped so missing data leaves no visible gap.
func (m Model) infoBodyLines(s model.Snapshot, width int) []string {
	sections := infoSections(s)
	labelW := infoLabelMin
	for _, sec := range sections {
		for _, r := range sec.rows {
			if n := len(r.label); n > labelW {
				labelW = n
			}
		}
	}
	var lines []string
	for _, sec := range sections {
		if len(sec.rows) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.renderInfoSection(sec, labelW, width)...)
	}
	return lines
}

// infoScrollable reports whether the info modal's body overflows the visible
// pane and therefore needs to advertise scroll keys in the footer. It is
// deliberately independent of m.footerRows() to avoid a cycle (footerRows
// builds viewHelp, which calls this). The info modal cannot have an active
// filter/search prompt, but an async status message can add one footer row.
func (m Model) infoScrollable() bool {
	if m.view != infoView {
		return false
	}
	s := m.selectedSnapshot()
	if s == nil {
		return false
	}
	w, h := m.effSize()
	footer := 1
	if m.statusMsg != "" {
		footer = 2
	}
	available := max(h-headerRows-2*gapRows-footer, 1)
	return len(m.infoBodyLines(*s, w)) > available
}

// scrollInfo adjusts the modal scroll offset by delta lines and clamps the
// result against the actual body extent so the model state always matches what
// the renderer will show. A no-op when the body already fits on screen.
func (m Model) scrollInfo(delta int) Model {
	s := m.selectedSnapshot()
	if s == nil {
		m.infoScroll = 0
		return m
	}
	w, _ := m.effSize()
	lines := m.infoBodyLines(*s, w)
	visible := m.modalVisible()
	if len(lines) <= visible {
		m.infoScroll = 0
		return m
	}
	bodyRows := max(visible-1, 1)
	m.infoScroll = clampModalScroll(m.infoScroll+delta, len(lines), bodyRows)
	return m
}

func (m Model) renderInfoSection(s infoSection, labelW, width int) []string {
	lines := make([]string, 0, 1+len(s.rows)*2)
	lines = append(lines, clip(m.styles.heading.Render(s.title), width))
	for _, r := range s.rows {
		lines = append(lines, m.renderInfoRow(r, labelW, width)...)
	}
	return lines
}

// renderInfoRow renders one labeled row. Single-value rows fit on one line;
// multi rows emit the label with the first value, then subsequent values as
// continuation lines padded to the value column so the list reads as a block.
func (m Model) renderInfoRow(r infoRow, labelW, width int) []string {
	if len(r.values) == 0 {
		return nil
	}
	prefix := "  "
	indent := prefix + strings.Repeat(" ", labelW) + "  "
	labelCell := m.styles.label.UnsetWidth().Render(padRight(r.label, labelW))
	first := clip(prefix+labelCell+"  "+r.values[0], width)
	if !r.multi || len(r.values) == 1 {
		return []string{first}
	}
	lines := make([]string, 0, len(r.values))
	lines = append(lines, first)
	for _, v := range r.values[1:] {
		lines = append(lines, clip(indent+v, width))
	}
	return lines
}

// infoSections is the pure data builder: it walks the snapshot record and
// returns the grouped rows in render order. Missing/empty values are filtered
// out at this layer so the renderer doesn't need to know which fields are
// optional.
func infoSections(s model.Snapshot) []infoSection {
	sections := []infoSection{
		{title: "Identity", rows: identityRows(s)},
		{title: "Source", rows: sourceRows(s)},
		{title: "Backup window", rows: backupWindowRows(s)},
		{title: "Churn", rows: churnRows(s)},
	}
	out := make([]infoSection, 0, len(sections))
	for _, sec := range sections {
		if len(sec.rows) > 0 {
			out = append(out, sec)
		}
	}
	return out
}

func identityRows(s model.Snapshot) []infoRow {
	rows := []infoRow{
		{label: "ID", values: []string{s.ID}},
	}
	if s.ShortID != "" {
		rows = append(rows, infoRow{label: "Short ID", values: []string{s.ShortID}})
	}
	if s.Parent != "" {
		rows = append(rows, infoRow{label: "Parent", values: []string{s.Parent}})
	}
	if s.Tree != "" {
		rows = append(rows, infoRow{label: "Tree", values: []string{s.Tree}})
	}
	if s.ProgramVersion != "" {
		rows = append(rows, infoRow{label: "Program", values: []string{s.ProgramVersion}})
	}
	return rows
}

func sourceRows(s model.Snapshot) []infoRow {
	var rows []infoRow
	if s.Hostname != "" {
		rows = append(rows, infoRow{label: "Hostname", values: []string{s.Hostname}})
	}
	if s.Username != "" {
		rows = append(rows, infoRow{label: "Username", values: []string{s.Username}})
	}
	if s.UID != nil {
		rows = append(rows, infoRow{label: "UID", values: []string{strconv.FormatUint(uint64(*s.UID), 10)}})
	}
	if s.GID != nil {
		rows = append(rows, infoRow{label: "GID", values: []string{strconv.FormatUint(uint64(*s.GID), 10)}})
	}
	if len(s.Tags) > 0 {
		rows = append(rows, infoRow{label: "Tags", values: []string{strings.Join(s.Tags, ", ")}})
	}
	if len(s.Paths) > 0 {
		rows = append(rows, infoRow{label: "Paths", values: s.Paths, multi: true})
	}
	if len(s.Excludes) > 0 {
		rows = append(rows, infoRow{label: "Excludes", values: s.Excludes, multi: true})
	}
	return rows
}

func backupWindowRows(s model.Snapshot) []infoRow {
	const layout = "2006-01-02 15:04:05"
	var rows []infoRow
	if !s.Time.IsZero() {
		rows = append(rows, infoRow{label: "Snapshot time", values: []string{s.Time.Format(layout)}})
	}
	if s.Summary == nil {
		return rows
	}
	if !s.Summary.BackupStart.IsZero() {
		rows = append(rows, infoRow{label: "Start", values: []string{s.Summary.BackupStart.Format(layout)}})
	}
	if !s.Summary.BackupEnd.IsZero() {
		rows = append(rows, infoRow{label: "End", values: []string{s.Summary.BackupEnd.Format(layout)}})
	}
	if d, ok := model.SnapshotBackupDuration(s); ok {
		rows = append(rows, infoRow{label: "Duration", values: []string{humanize.Duration(d)}})
	}
	return rows
}

func churnRows(s model.Snapshot) []infoRow {
	if s.Summary == nil {
		return nil
	}
	sum := s.Summary
	rows := []infoRow{
		{label: "Total bytes", values: []string{humanize.Bytes(sum.TotalBytesProcessed)}},
	}
	if sum.DataAdded != nil {
		rows = append(rows, infoRow{label: "Data added", values: []string{humanize.Bytes(*sum.DataAdded)}})
	}
	if sum.DataAddedPacked != nil {
		rows = append(rows, infoRow{label: "Data added packed", values: []string{humanize.Bytes(*sum.DataAddedPacked)}})
	}
	if sum.DataBlobs != nil {
		rows = append(rows, infoRow{label: "Data blobs", values: []string{strconv.FormatInt(*sum.DataBlobs, 10)}})
	}
	if sum.TreeBlobs != nil {
		rows = append(rows, infoRow{label: "Tree blobs", values: []string{strconv.FormatInt(*sum.TreeBlobs, 10)}})
	}
	if sum.FilesNew != nil {
		rows = append(rows, infoRow{label: "Files new", values: []string{strconv.FormatUint(*sum.FilesNew, 10)}})
	}
	if sum.FilesChanged != nil {
		rows = append(rows, infoRow{label: "Files changed", values: []string{strconv.FormatUint(*sum.FilesChanged, 10)}})
	}
	if sum.FilesUnmodified != nil {
		rows = append(rows, infoRow{label: "Files unmodified", values: []string{strconv.FormatUint(*sum.FilesUnmodified, 10)}})
	}
	if sum.TotalFilesProcessed != nil {
		rows = append(rows, infoRow{label: "Files total", values: []string{strconv.FormatUint(*sum.TotalFilesProcessed, 10)}})
	}
	if sum.DirsNew != nil {
		rows = append(rows, infoRow{label: "Dirs new", values: []string{strconv.FormatUint(*sum.DirsNew, 10)}})
	}
	if sum.DirsChanged != nil {
		rows = append(rows, infoRow{label: "Dirs changed", values: []string{strconv.FormatUint(*sum.DirsChanged, 10)}})
	}
	if sum.DirsUnmodified != nil {
		rows = append(rows, infoRow{label: "Dirs unmodified", values: []string{strconv.FormatUint(*sum.DirsUnmodified, 10)}})
	}
	// Restic does not emit a total-dirs counter; derive it only when all three
	// dir buckets are present so the modal can show a faithful total alongside
	// the file total without inventing a number.
	if sum.DirsNew != nil && sum.DirsChanged != nil && sum.DirsUnmodified != nil {
		total := *sum.DirsNew + *sum.DirsChanged + *sum.DirsUnmodified
		rows = append(rows, infoRow{label: "Dirs total", values: []string{strconv.FormatUint(total, 10)}})
	}
	return rows
}
