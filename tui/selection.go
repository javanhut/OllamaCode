package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

type selection struct {
	active bool
	anchor int // content line index
	cursor int // content line index
}

func (m *Model) contentLineAt(x, y int) int {
	vpW := max(m.width-m.sidebarSpace(), 10)
	if x >= vpW {
		return -1 // Clicked in the sidebar or the gap
	}

	headerH := lipgloss.Height(m.headerView())
	viewportY := y - headerH
	if viewportY < 0 {
		return -1
	}
	if viewportY >= m.viewport.Height() {
		return -2
	}
	return m.transcriptLineAtVisualOffset(m.viewport.YOffset() + viewportY)
}

func (m *Model) transcriptLineAtVisualOffset(offset int) int {
	content := m.transcript.String()
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return 0
	}

	width := m.viewport.Width()
	if width <= 0 {
		width = m.width
	}
	if width <= 0 {
		width = 1
	}

	visual := 0
	for i, line := range lines {
		lineHeight := 1
		if m.viewport.SoftWrap {
			lineWidth := ansi.StringWidth(line)
			if lineWidth > 0 {
				lineHeight = (lineWidth + width - 1) / width
			}
		}
		if offset < visual+lineHeight {
			return i
		}
		visual += lineHeight
	}
	return len(lines) - 1
}

func (m *Model) selectionRange() (int, int, []string, bool) {
	if !m.sel.active {
		return 0, 0, nil, false
	}
	content := m.transcript.String()
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return 0, 0, nil, false
	}
	s, e := m.sel.anchor, m.sel.cursor
	if s > e {
		s, e = e, s
	}
	last := len(lines) - 1
	if s < 0 {
		s = 0
	}
	if s > last {
		s = last
	}
	if e < 0 {
		e = 0
	}
	if e > last {
		e = last
	}
	return s, e, lines, true
}

func (m *Model) selectedTranscriptLine(line int) bool {
	s, e, _, ok := m.selectionRange()
	return ok && line >= s && line <= e
}

func (m *Model) applySelectionHighlight() {
	m.viewport.ClearHighlights()
}

func (m *Model) copySelection() {
	s, e, lines, ok := m.selectionRange()
	if !ok {
		return
	}
	plain := ansi.Strip(strings.Join(lines[s:e+1], "\n"))
	if strings.TrimSpace(plain) == "" {
		return
	}
	if err := clipboard.WriteAll(plain); err != nil {
		m.toast = fmt.Sprintf("clipboard error: %v", err)
		return
	}
	m.toast = fmt.Sprintf("copied %d chars", len(plain))
}
