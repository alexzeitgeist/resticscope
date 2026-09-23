package tui

import (
	"context"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// The diff info screen reads the selected change's record from each snapshot
// with `restic cat tree`, so it can name the fields restic's marker leaves
// out. Records live only while the screen is open.

// diffInfoSide is one snapshot's column. want is false when the change marker
// says the path is absent there, so no lookup runs for it.
type diffInfoSide struct {
	want bool
	res  app.DiffNodeSide
}

// node returns the side's record, or nil when it is absent, missing, or failed.
func (s diffInfoSide) node() *model.TreeNode {
	if !s.want || !s.res.Found || s.res.Err != nil {
		return nil
	}
	return &s.res.Node
}

// openDiffInfo starts reading the selected row's records. A row marked only +
// or - exists in one snapshot, so only that side is read.
func (m Model) openDiffInfo() (Model, tea.Cmd) {
	r := m.selectedDiffRow()
	if r == nil {
		return m, nil
	}
	m = m.dropDiffInfo()
	ctx, cancel := context.WithCancel(m.ctx)
	m.diffInfoCancel = cancel
	m.diffInfoRow = *r
	m.diffInfoFirst = diffInfoSide{want: r.Kinds != model.KindAdded}
	m.diffInfoSecond = diffInfoSide{want: r.Kinds != model.KindRemoved}
	m.statusMsg = ""
	m.view = diffInfoView

	firstID, secondID := "", ""
	if m.diffInfoFirst.want {
		firstID = m.diffOlder.ID
	}
	if m.diffInfoSecond.want {
		secondID = m.diffNewer.ID
	}
	a, repo, p, gen := m.app, m.diffRepo, r.Path, m.diffInfoGen
	return m, func() tea.Msg {
		first, second := a.DiffNodes(ctx, repo, p, firstID, secondID)
		return diffInfoMsg{gen: gen, first: first, second: second}
	}
}

// applyDiffInfoMsg installs the records of the lookup the screen is waiting on.
func (m Model) applyDiffInfoMsg(msg diffInfoMsg) Model {
	if msg.gen != m.diffInfoGen || m.diffInfoCancel == nil {
		return m
	}
	m.diffInfoCancel()
	m.diffInfoCancel = nil
	m.diffInfoFirst.res = msg.first
	m.diffInfoSecond.res = msg.second
	return m
}

func (m Model) diffInfoLoading() bool {
	return m.diffInfoCancel != nil
}

// handleDiffInfoKey closes the screen on esc or i and scrolls an overflowing
// body; q reaches closeDiffInfo through handleQuitKey.
func (m Model) handleDiffInfoKey(msg tea.KeyPressMsg) Model {
	switch {
	case key.Matches(msg, m.keys.Back), key.Matches(msg, m.keys.Info):
		m = m.closeDiffInfo()
	case key.Matches(msg, m.keys.Up):
		m = m.scrollDiffInfo(-1)
	case key.Matches(msg, m.keys.Down):
		m = m.scrollDiffInfo(1)
	case key.Matches(msg, m.keys.PageUp):
		m = m.scrollDiffInfo(-m.modalVisible())
	case key.Matches(msg, m.keys.PageDown):
		m = m.scrollDiffInfo(m.modalVisible())
	}
	return m
}

// closeDiffInfo returns to the diff listing with its cursor unchanged.
func (m Model) closeDiffInfo() Model {
	m = m.dropDiffInfo()
	m.view = snapshotDiffView
	return m
}

// dropDiffInfo cancels a pending lookup, rejects its late result, and clears
// the records.
func (m Model) dropDiffInfo() Model {
	m.diffInfoGen++
	if m.diffInfoCancel != nil {
		m.diffInfoCancel()
		m.diffInfoCancel = nil
	}
	m.diffInfoRow = model.DiffRow{}
	m.diffInfoFirst = diffInfoSide{}
	m.diffInfoSecond = diffInfoSide{}
	m.diffInfoScroll = 0
	return m
}

func (m Model) scrollDiffInfo(delta int) Model {
	w, _ := m.effSize()
	m.diffInfoScroll = modalScrollBy(m.diffInfoScroll, delta, len(m.diffInfoBodyLines(w)), m.modalVisible())
	return m
}
