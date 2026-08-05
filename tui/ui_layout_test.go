package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
)

// Modal headers must fit on one line: the title and its hint share a row, and a
// content width even one column too wide wraps the hint onto its own line and
// pushes every modal a line taller.
func TestModalHeaderFitsOneLine(t *testing.T) {
	for _, w := range []int{60, 90, 140} {
		mm, _ := New().Update(tea.WindowSizeMsg{Width: w, Height: 40})
		m := mm.(*Model)
		box := modalStyle.Width(m.modalWidth()).Render(m.modalHeader("Tool wants to run", "n=deny", m.modalInner()))
		// border + padding + 1 content row + padding + border
		if got := strings.Count(box, "\n") + 1; got != 5 {
			t.Errorf("width %d: header box is %d lines, want 5 (header wrapped)", w, got)
		}
	}
}

func TestHelpModalKeepsTitleAndHintOnHeaderRow(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := mm.(*Model)
	m.helpViewport.SetContent(m.helpContent(m.helpViewport.Width()))
	lines := strings.Split(m.helpModal(), "\n")
	if len(lines) < 3 {
		t.Fatal("modal too short")
	}
	if !strings.Contains(lines[2], "Help") || !strings.Contains(lines[2], "esc") {
		t.Errorf("header row = %q, want title and hint together", stripANSI(lines[2]))
	}
	if h := len(lines); h > 30 {
		t.Errorf("modal is %d lines on a 30-line screen", h)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// The sidebar box must always close, at every height and however much content
// it holds — overflowing content used to clip the bottom border away.
func TestSidebarBoxAlwaysCloses(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m := mm.(*Model)
	m.todos.set([]todoItem{
		{Content: "a long task description that wraps the panel", Status: todoInProgress},
		{Content: "another task", Status: todoPending},
		{Content: "third task", Status: todoPending},
	})
	for h := 6; h <= 40; h++ {
		box := m.sidebarView(h)
		lines := strings.Split(box, "\n")
		if len(lines) != h {
			t.Errorf("height %d: rendered %d lines", h, len(lines))
			continue
		}
		if !strings.Contains(lines[len(lines)-1], "╰") {
			t.Errorf("height %d: bottom border missing, last line = %q", h, stripANSI(lines[len(lines)-1]))
		}
	}
}

// The completion menu is windowed: a bare "/" matches every command, and the
// menu may never grow past its cap or the transcript disappears behind it.
func TestSlashMenuIsWindowed(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	m := typeKeys(t, mm.(*Model), "/")
	if len(m.slashSuggestions) <= slashMenuRows {
		t.Fatal("expected more matches than the row cap")
	}
	rows := strings.Count(m.slashSuggestionsView(), "\n") + 1
	if want := slashMenuRows + 4; rows != want { // + rule, caption, two borders
		t.Errorf("menu drew %d rows, want %d", rows, want)
	}
	// The highlight has to stay inside the window as it moves past the cap.
	for i := 0; i < slashMenuRows+3; i++ {
		m = press(t, m, tea.KeyDown, 0)
	}
	view := stripANSI(m.slashSuggestionsView())
	if !strings.Contains(view, m.slashSuggestions[m.slashSelected]) {
		t.Errorf("selection %q scrolled out of view:\n%s", m.slashSuggestions[m.slashSelected], view)
	}
}

// An empty session still says something — the welcome panel is off by default.
func TestEmptySessionIsNotBlank(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	m := mm.(*Model)
	m.refreshTranscript()
	if !strings.Contains(stripANSI(m.viewChat()), "/help") {
		t.Error("empty chat renders no orientation hints")
	}
}

// The completion menu is a closed box that never runs past the terminal.
func TestSlashMenuBoxFits(t *testing.T) {
	for _, size := range [][2]int{{120, 34}, {80, 24}, {64, 16}, {50, 12}} {
		mm, _ := New().Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m := typeKeys(t, mm.(*Model), "/")
		lines := strings.Split(m.slashSuggestionsView(), "\n")
		if !strings.Contains(lines[0], "╭") || !strings.Contains(lines[len(lines)-1], "╰") {
			t.Errorf("%v: menu box not closed:\n%s", size, stripANSI(m.slashSuggestionsView()))
		}
		for _, l := range lines {
			if w := lipgloss.Width(l); w > size[0] {
				t.Errorf("%v: menu row is %d wide", size, w)
				break
			}
		}
	}
}

// The header is one row at any width: it drops the meta, then the model name,
// rather than overflowing or wrapping.
func TestHeaderIsOneRowAtEveryWidth(t *testing.T) {
	for _, w := range []int{160, 120, 80, 60, 46, 34, 20} {
		mm, _ := New().Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m := mm.(*Model)
		m.modelName = "some-really-long-model-name:120b-cloud"
		m.gitBranch = "feature/a-branch-name-that-does-not-stop"
		m.totalTokens, m.contextLimit = 41000, 128000

		lines := strings.Split(m.headerView(), "\n")
		if len(lines) != 2 { // title row + rule
			t.Errorf("width %d: header is %d lines", w, len(lines))
			continue
		}
		for _, l := range lines {
			if got := lipgloss.Width(l); got != w {
				t.Errorf("width %d: header row is %d wide: %q", w, got, stripANSI(l))
			}
		}
		// The mode indicator is the one thing that must survive every squeeze,
		// even if it shrinks to its initial.
		if !strings.Contains(stripANSI(lines[0]), "E") {
			t.Errorf("width %d: mode chip dropped: %q", w, stripANSI(lines[0]))
		}
	}
}

// The whole screen must fit the terminal at any size: every row within the
// width, and exactly as many rows as the terminal has.
func TestChatViewFitsEveryTerminalSize(t *testing.T) {
	sizes := [][2]int{{20, 10}, {30, 12}, {40, 16}, {50, 20}, {59, 24}, {80, 8}, {100, 30}, {200, 60}}
	for _, size := range sizes {
		mm, _ := New().Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m := mm.(*Model)
		m.modelName = "kimi-k3:cloud"
		m.history = []api.Message{
			{Role: "user", Content: "hello there this is a longish message"},
			{Role: "assistant", Content: "and a longish reply that will wrap on narrow terminals"},
		}
		m.toast = "loaded model kimi-k3:cloud"
		m.refreshTranscript()

		lines := strings.Split(m.viewChat(), "\n")
		if len(lines) != size[1] {
			t.Errorf("%v: %d rows, want %d", size, len(lines), size[1])
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > size[0] {
				t.Errorf("%v: row %d is %d wide: %q", size, i, w, stripANSI(l))
				break
			}
		}
	}
}

// Scrolled up, the transcript says so — and the cue is an overlay, so it can
// never change the layout that decides whether it appears.
func TestScrollCueDoesNotResizeTheLayout(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m := mm.(*Model)
	for i := range 40 {
		m.history = append(m.history, api.Message{Role: "user", Content: fmt.Sprintf("line %d", i)})
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()
	atBottom := strings.Split(m.viewChat(), "\n")
	if strings.Contains(stripANSI(strings.Join(atBottom, "\n")), "more line") {
		t.Error("cue shown while pinned to the bottom")
	}
	m.viewport.SetYOffset(0)
	scrolled := strings.Split(m.viewChat(), "\n")
	if !strings.Contains(stripANSI(strings.Join(scrolled, "\n")), "more line") {
		t.Error("no cue after scrolling up")
	}
	if len(atBottom) != len(scrolled) {
		t.Errorf("cue changed the row count: %d vs %d", len(atBottom), len(scrolled))
	}
}

func TestResizeKeepsTranscriptPinnedToBottom(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m := mm.(*Model)
	for i := range 80 {
		m.history = append(m.history, api.Message{Role: "user", Content: fmt.Sprintf("line %d", i)})
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()

	mm, _ = m.Update(tea.WindowSizeMsg{Width: 72, Height: 15})
	m = mm.(*Model)
	if !m.viewport.AtBottom() {
		t.Fatalf("resize moved a bottom-pinned transcript to offset %d", m.viewport.YOffset())
	}
}

func TestResizePreservesManualScrollPosition(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m := mm.(*Model)
	for i := range 80 {
		m.history = append(m.history, api.Message{Role: "user", Content: fmt.Sprintf("line %d", i)})
	}
	m.refreshTranscript()
	m.viewport.SetYOffset(5)

	mm, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 18})
	m = mm.(*Model)
	if m.viewport.YOffset() != 5 {
		t.Fatalf("resize changed manual scroll offset to %d, want 5", m.viewport.YOffset())
	}
}

// Sealed turns render once; only the open turn is rebuilt each frame.
func TestSealedTurnsAreCached(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := mm.(*Model)
	m.history = []api.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: "second answer"},
	}
	m.refreshTranscript()
	first := m.transcript.String()
	if len(m.turnCache) != 1 { // only turn one is sealed; turn two is still open
		t.Errorf("cached %d turns, want 1", len(m.turnCache))
	}
	m.refreshTranscript()
	if m.transcript.String() != first {
		t.Error("cached render differs from a fresh one")
	}
	// A setting that changes rendering must invalidate.
	m.expandTools = true
	m.refreshTranscript()
	if m.turnCacheStamp == "" {
		t.Error("cache stamp not tracked")
	}
	if !strings.Contains(stripANSI(m.transcript.String()), "first answer") {
		t.Error("turn dropped after invalidation")
	}
}
