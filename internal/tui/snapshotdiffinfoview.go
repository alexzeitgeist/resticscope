package tui

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/lipgloss/v2"
)

const (
	// diffInfoLabelW fits the longest field label, "Link target".
	diffInfoLabelW = 11

	// Times print to the second unless two that differ would print the same.
	diffInfoTimeLayout        = "2006-01-02 15:04:05"
	diffInfoPreciseTimeLayout = "2006-01-02 15:04:05.000000000"
)

// diffInfoField is one table row. A span row compares both records in one
// value. changed marks a difference between two present records, and name is
// how the verdict lists it.
type diffInfoField struct {
	label, name   string
	first, second string
	span          string
	changed       bool
}

func (m Model) diffInfoTitle() string {
	return m.styles.title.Render("info: " + m.diffRepo + " · " +
		diffSnapshotLabel(m.diffOlder) + " → " + diffSnapshotLabel(m.diffNewer))
}

func (m Model) diffInfoBody() string {
	w, _ := m.effSize()
	return m.modalWindow(m.diffInfoBodyLines(w), m.diffInfoScroll, w)
}

// diffInfoScrollable reports whether the info body overflows the pane.
func (m Model) diffInfoScrollable() bool {
	if m.view != diffInfoView {
		return false
	}
	w, _ := m.effSize()
	return m.modalOverflows(len(m.diffInfoBodyLines(w)))
}

// diffInfoBodyLines lays out the path, a one-line verdict, lookup problems, the
// side-by-side table, and notes on how restic chose its marker.
func (m Model) diffInfoBodyLines(width int) []string {
	r := m.diffInfoRow
	label := r.Path
	if r.IsDir {
		label += "/"
	}
	lines := []string{m.pathLine("Path", label, width)}
	if m.diffInfoLoading() {
		return append(lines, clip(m.styles.meta.Render("  loading… reading "+m.diffInfoReading()+" · esc cancels"), width))
	}
	first, second := m.diffInfoFirst.node(), m.diffInfoSecond.node()
	fields := diffInfoFields(first, second)
	lines = append(lines, clip("  "+m.diffInfoVerdict(fields, first, second), width))
	lines = append(lines, m.diffInfoProblems(width)...)
	lines = append(lines, "")
	lines = append(lines, m.diffInfoTable(fields, width)...)
	if notes := diffInfoNotes(r, first, second); len(notes) > 0 {
		lines = append(lines, "")
		for _, n := range notes {
			for l := range strings.SplitSeq(wrapWords(n, width-2), "\n") {
				lines = append(lines, clip(m.styles.meta.Render("  "+l), width))
			}
		}
	}
	return lines
}

// diffInfoReading names what a pending lookup reads.
func (m Model) diffInfoReading() string {
	switch {
	case !m.diffInfoFirst.want:
		return diffSnapshotShort(m.diffNewer)
	case !m.diffInfoSecond.want:
		return diffSnapshotShort(m.diffOlder)
	}
	return "both snapshots"
}

func diffSnapshotShort(s model.Snapshot) string {
	if s.ShortID != "" {
		return s.ShortID
	}
	return shortID(s.ID)
}

// diffInfoVerdict restates the row's marker and, when both records are present,
// lists the fields that actually differ.
func (m Model) diffInfoVerdict(fields []diffInfoField, first, second *model.TreeNode) string {
	r := m.diffInfoRow
	marker, text := "", "contains changes"
	if r.Modifier != "" {
		marker = diffTypeStyle(r.Type, m.styles).Render(r.Modifier) + " "
		text = diffChangeMeaning(model.PrimaryChangeType(r.Kinds))
	}
	if first != nil && second != nil {
		var changed []string
		for _, f := range fields {
			if f.changed {
				changed = append(changed, f.name)
			}
		}
		if len(changed) == 0 {
			text += " · every recorded field matches"
		} else {
			text += " · differs in " + joinWords(changed)
		}
	}
	return marker + m.styles.meta.Render(text)
}

// diffChangeMeaning matches the marker table in docs/browsing.md.
func diffChangeMeaning(t model.ChangeType) string {
	switch t {
	case model.ChangeAdded:
		return "only in the second snapshot"
	case model.ChangeRemoved:
		return "only in the first snapshot"
	case model.ChangeModified:
		return "contents modified"
	case model.ChangeMetadataOnly:
		return "metadata only"
	case model.ChangeTypeChanged:
		return "type changed"
	case model.ChangeBitrot:
		return "bitrot reported by restic"
	}
	return "changed"
}

// joinWords lists words as prose: "a", "a and b", "a, b, and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
}

// diffInfoProblems reports requested records that could not be shown.
func (m Model) diffInfoProblems(width int) []string {
	var out []string
	for _, s := range []struct {
		side diffInfoSide
		snap model.Snapshot
	}{{m.diffInfoFirst, m.diffOlder}, {m.diffInfoSecond, m.diffNewer}} {
		switch {
		case !s.side.want:
		case s.side.res.Err != nil:
			out = append(out, clip(m.styles.errText.Render("  "+diffSnapshotShort(s.snap)+": "+firstLine(s.side.res.Err.Error())), width))
		case !s.side.res.Found:
			out = append(out, clip(m.styles.meta.Render("  "+diffSnapshotShort(s.snap)+": no entry at this path"), width))
		}
	}
	return out
}

// diffInfoTable renders the fields side by side, marking differences in the
// gutter. The first column fits its widest value, up to half the room, so the
// two records stay close on a wide terminal. A side without a record shows a
// dash.
func (m Model) diffInfoTable(fields []diffInfoField, width int) []string {
	const gutter, gap = 2, 2
	room := width - gutter - diffInfoLabelW - 2*gap
	colW := lipgloss.Width(diffSnapshotShort(m.diffOlder))
	for _, f := range fields {
		if f.span == "" {
			colW = max(colW, lipgloss.Width(f.first))
		}
	}
	colW = min(colW, max(room/2, 12))
	secondW := max(room-colW, 12)
	header := "  " + padRight("Field", diffInfoLabelW) + "  " +
		padRight(diffSnapshotShort(m.diffOlder), colW) + "  " + diffSnapshotShort(m.diffNewer)
	lines := []string{clip(m.styles.dim.Render(header), width)}
	for _, f := range fields {
		mark := "  "
		if f.changed {
			mark = m.diffInfoFieldStyle(f.name).Render("≠") + " "
		}
		labelCell := m.styles.label.UnsetWidth().Render(padRight(f.label, diffInfoLabelW))
		values := f.span
		if f.span == "" {
			values = padRight(m.diffInfoCell(f.first, colW), colW) + "  " + m.diffInfoCell(f.second, secondW)
		}
		lines = append(lines, clip(mark+labelCell+"  "+values, width))
	}
	return lines
}

// diffInfoFieldStyle colors a difference like the marker restic would give it.
func (m Model) diffInfoFieldStyle(name string) lipgloss.Style {
	switch name {
	case "contents":
		return m.styles.chgModified
	case "type":
		return m.styles.chgTypeChanged
	}
	return m.styles.chgMetadata
}

func (m Model) diffInfoCell(v string, width int) string {
	if v == "" {
		return m.styles.dim.Render("—")
	}
	return truncateWidth(v, width)
}

// diffInfoFields builds the table rows for whichever records are present. Rows
// that neither record uses are left out, and atime appears only where restic
// stored a real access time rather than its default copy of mtime.
func diffInfoFields(a, b *model.TreeNode) []diffInfoField {
	both := a != nil && b != nil
	var c model.NodeChanges
	if both {
		c = model.CompareTreeNodes(*a, *b)
	}
	either := func(pred func(*model.TreeNode) bool) bool {
		return (a != nil && pred(a)) || (b != nil && pred(b))
	}
	var out []diffInfoField
	add := func(label, name string, changed bool, value func(*model.TreeNode) string) {
		f := diffInfoField{label: label, name: name, changed: both && changed}
		if a != nil {
			f.first = value(a)
		}
		if b != nil {
			f.second = value(b)
		}
		out = append(out, f)
	}
	// samePrint reports two differing values that would print identically.
	samePrint := func(changed bool, value func(*model.TreeNode) string) bool {
		return both && changed && value(a) == value(b)
	}
	isFile := func(n *model.TreeNode) bool { return n.Type == model.NodeTypeFile }
	isDir := func(n *model.TreeNode) bool { return n.Type == model.NodeTypeDir }

	add("Type", "type", c.Type, func(n *model.TreeNode) string { return n.Type })
	if either(isFile) {
		exact := samePrint(c.Size, func(n *model.TreeNode) string { return diffInfoSize(n.Size, false) })
		add("Size", "size", c.Size, func(n *model.TreeNode) string { return diffInfoSize(n.Size, exact) })
	}
	add("Mode", "mode", c.Mode, func(n *model.TreeNode) string { return n.Mode.String() })
	add("Owner", "owner", c.Owner, func(n *model.TreeNode) string { return diffInfoID(n.User, n.UID) })
	add("Group", "group", c.Group, func(n *model.TreeNode) string { return diffInfoID(n.Group, n.GID) })
	addTime := func(label string, changed bool, get func(*model.TreeNode) time.Time) {
		precise := samePrint(changed, func(n *model.TreeNode) string { return diffInfoTime(get(n), false) })
		add(label, label, changed, func(n *model.TreeNode) string { return diffInfoTime(get(n), precise) })
	}
	addTime("mtime", c.ModTime, func(n *model.TreeNode) time.Time { return n.ModTime })
	addTime("ctime", c.ChangeTime, func(n *model.TreeNode) time.Time { return n.ChangeTime })
	if either(func(n *model.TreeNode) bool { return !n.AccessTime.Equal(n.ModTime) }) {
		addTime("atime", c.AccessTime, func(n *model.TreeNode) time.Time { return n.AccessTime })
	}
	addNumber := func(label, name string, changed bool, get func(*model.TreeNode) uint64) {
		if either(func(n *model.TreeNode) bool { return get(n) != 0 }) {
			add(label, name, changed, func(n *model.TreeNode) string { return strconv.FormatUint(get(n), 10) })
		}
	}
	addNumber("Inode", "inode", c.Inode, func(n *model.TreeNode) uint64 { return n.Inode })
	addNumber("Links", "links", c.Links, func(n *model.TreeNode) uint64 { return n.Links })
	addNumber("Device ID", "device ID", c.DeviceID, func(n *model.TreeNode) uint64 { return n.DeviceID })
	addNumber("Device", "device", c.Device, func(n *model.TreeNode) uint64 { return n.Device })
	if either(func(n *model.TreeNode) bool { return n.LinkTarget != "" }) {
		add("Link target", "link target", c.LinkTarget, func(n *model.TreeNode) string { return n.LinkTarget })
	}
	if either(func(n *model.TreeNode) bool { return len(n.Xattrs) > 0 }) {
		add("Xattrs", "xattrs", c.Xattrs, func(n *model.TreeNode) string {
			// Sorted like the comparison, which ignores stored order.
			names := make([]string, 0, len(n.Xattrs))
			for _, x := range n.Xattrs {
				names = append(names, x.Name)
			}
			slices.Sort(names)
			return diffInfoNames(names)
		})
	}
	if either(func(n *model.TreeNode) bool { return len(n.GenericAttrs) > 0 }) {
		add("Attributes", "attributes", c.GenericAttrs, func(n *model.TreeNode) string {
			return diffInfoNames(slices.Sorted(maps.Keys(n.GenericAttrs)))
		})
	}
	if either(func(n *model.TreeNode) bool { return n.Error != "" }) {
		add("Error", "error", c.Error, func(n *model.TreeNode) string { return n.Error })
	}
	if both && (either(isFile) || either(isDir)) {
		changed := c.Content || c.Tree
		span := "identical"
		switch {
		case changed && isDir(a) && isDir(b):
			span = "changed below"
		case changed:
			span = "changed"
		}
		out = append(out, diffInfoField{label: "Contents", name: "contents", span: span, changed: changed})
	}
	return out
}

func diffInfoSize(n uint64, exact bool) string {
	s := humanize.Bytes(int64(n)) //nolint:gosec // restic sizes are file lengths, far below MaxInt64
	if exact {
		s += " (" + strconv.FormatUint(n, 10) + " B)"
	}
	return s
}

// diffInfoID shows a user or group name beside its numeric ID.
func diffInfoID(name string, id uint32) string {
	n := strconv.FormatUint(uint64(id), 10)
	if name == "" {
		return n
	}
	return name + " (" + n + ")"
}

func diffInfoTime(t time.Time, precise bool) string {
	switch {
	case t.IsZero():
		return ""
	case precise:
		return t.Format(diffInfoPreciseTimeLayout)
	}
	return t.Format(diffInfoTimeLayout)
}

func diffInfoNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// diffInfoNotes explains verdicts that restic's marker would make misleading.
func diffInfoNotes(r model.DiffRow, a, b *model.TreeNode) []string {
	if a == nil || b == nil {
		return nil
	}
	c := model.CompareTreeNodes(*a, *b)
	own := c.OwnFieldsOnly().Any()
	var notes []string
	if r.IsDir && r.Kinds&model.KindMetadata != 0 && c.Tree && !own {
		notes = append(notes, "restic marks a directory U when anything below it changes; its own metadata is unchanged.")
	}
	if c == (model.NodeChanges{ChangeTime: true}) {
		notes = append(notes, "Only ctime changed, so something touched the inode without changing anything else restic records: "+
			"a chmod or chown to the values it already had, a rename, or a hard link added and removed.")
	}
	if r.Kinds&model.KindBitrot != 0 && c.Content && !own {
		notes = append(notes, "Only the contents changed while every other field stayed the same, so restic suspects bitrot.")
	}
	// Values are never shown, so name the attributes whose names match on both
	// sides but whose values do not.
	if names := model.XattrValuesDiffer(a.Xattrs, b.Xattrs); len(names) > 0 {
		notes = append(notes, "Extended attribute values differ: "+strings.Join(names, ", "))
	}
	if names := model.GenericAttrValuesDiffer(a.GenericAttrs, b.GenericAttrs); len(names) > 0 {
		notes = append(notes, "Attribute values differ: "+strings.Join(names, ", "))
	}
	return notes
}
