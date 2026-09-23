package tui

import (
	"strconv"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// The snapshot-info modal renders the selected restic snapshot record. It
// computes its label width independently because its labels are unusually long.

// infoLabelMin prevents an empty group from collapsing the value column into
// the gutter.
const infoLabelMin = 12

// infoRow is a label/value pair; multi places each value on its own line.
type infoRow struct {
	label  string
	values []string
	multi  bool
}

// infoSection groups rows under a heading. The renderer omits empty sections,
// including summary data absent from older restic versions.
type infoSection struct {
	title string
	rows  []infoRow
}

// infoTitle identifies the detail repository and selected snapshot. It uses
// detailName because that survives a refresh which drops the selected row.
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
	return m.modalWindow(m.infoBodyLines(*s, w), m.infoScroll, w)
}

// infoBodyLines builds a flat, windowable body, separating non-empty sections
// with one blank line.
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

// infoScrollable reports whether the info body overflows the pane.
func (m Model) infoScrollable() bool {
	if m.view != infoView {
		return false
	}
	s := m.selectedSnapshot()
	if s == nil {
		return false
	}
	w, _ := m.effSize()
	return m.modalOverflows(len(m.infoBodyLines(*s, w)))
}

// scrollInfo adjusts and clamps the modal scroll offset. It resets the offset
// when no snapshot is selected or the body fits on screen.
func (m Model) scrollInfo(delta int) Model {
	s := m.selectedSnapshot()
	if s == nil {
		m.infoScroll = 0
		return m
	}
	w, _ := m.effSize()
	m.infoScroll = modalScrollBy(m.infoScroll, delta, len(m.infoBodyLines(*s, w)), m.modalVisible())
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

// renderInfoRow renders one labeled row. Multi-value rows indent continuation
// lines to the value column.
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

// infoSections groups snapshot fields in render order and filters empty
// sections before they reach the renderer.
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
	// Restic has no total-dirs counter, so derive it only when every bucket is
	// present.
	if sum.DirsNew != nil && sum.DirsChanged != nil && sum.DirsUnmodified != nil {
		total := *sum.DirsNew + *sum.DirsChanged + *sum.DirsUnmodified
		rows = append(rows, infoRow{label: "Dirs total", values: []string{strconv.FormatUint(total, 10)}})
	}
	return rows
}
