package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The sidebar sits beside the transcript, so it must occupy exactly
// sidebarSpace()-len(sidebarGap) columns and exactly the height it's given —
// otherwise the JoinHorizontal row overflows the terminal and the chat wraps.
func TestSidebarViewFitsItsBox(t *testing.T) {
	m := &Model{width: 120, height: 40, mode: WriteMode, totalTokens: 4000, contextLimit: 32000}
	m.todos = &todoList{}
	m.todos.set([]todoItem{
		{Content: "read the layout code", Status: todoCompleted},
		{Content: "a step whose text is far too long to ever fit in the panel", Status: todoInProgress},
	})
	m.dreams = []dream{{summary: "noticed the notes viewport was mis-sized"}}
	m.asleep = true

	const h = 30
	view := m.sidebarView(h)
	if got := lipgloss.Height(view); got != h {
		t.Errorf("height = %d, want %d", got, h)
	}
	if got, want := lipgloss.Width(view), m.sidebarSpace()-len(sidebarGap); got != want {
		t.Errorf("width = %d, want %d", got, want)
	}
	for _, want := range []string{"WRITE", "Status", "ASLEEP", "4k / 32k ctx", "Tasks (1/2)", "Dreams (1)"} {
		if !strings.Contains(view, want) {
			t.Errorf("sidebar missing %q:\n%s", want, view)
		}
	}
}

func TestSidebarHiddenWhenNarrow(t *testing.T) {
	m := &Model{width: 50, height: 40, mode: ExploreMode}
	m.todos = &todoList{}
	if m.sidebarSpace() != 0 {
		t.Errorf("sidebarSpace = %d, want 0", m.sidebarSpace())
	}
	if v := m.sidebarView(30); v != "" {
		t.Errorf("sidebarView = %q, want empty", v)
	}
}
