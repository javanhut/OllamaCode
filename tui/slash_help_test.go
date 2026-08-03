package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func typeKeys(t *testing.T, m *Model, s string) *Model {
	t.Helper()
	for _, r := range s {
		mm, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mm.(*Model)
	}
	return m
}

func press(t *testing.T, m *Model, code rune, mod tea.KeyMod) *Model {
	t.Helper()
	mm, _ := m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
	return mm.(*Model)
}

func newSized(t *testing.T) *Model {
	t.Helper()
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return mm.(*Model)
}

// Tab completes the highlighted entry, not the next one down the list.
func TestTabCompletesHighlightedSuggestion(t *testing.T) {
	m := typeKeys(t, newSized(t), "/mod")
	if !m.slashVisible || m.slashSuggestions[0] != "/model" {
		t.Fatalf("expected /model highlighted first, got %v", m.slashSuggestions)
	}
	m = press(t, m, tea.KeyTab, 0)
	if got := m.input.Value(); got != "/model" {
		t.Fatalf("tab completed to %q, want /model", got)
	}
	if m.slashVisible {
		t.Fatal("menu should close after completing")
	}

	// ↓ then tab takes the second entry.
	m = typeKeys(t, newSized(t), "/mod")
	m = press(t, m, tea.KeyDown, 0)
	m = press(t, m, tea.KeyTab, 0)
	if got := m.input.Value(); got != "/models" {
		t.Fatalf("after ↓, tab completed to %q, want /models", got)
	}
}

// A fully-typed command runs itself instead of being replaced by a longer
// lookalike still showing in the menu.
func TestEnterRunsExactlyTypedCommand(t *testing.T) {
	m := typeKeys(t, newSized(t), "/mode")
	if !m.slashVisible {
		t.Fatal("expected /model, /models still suggested")
	}
	m = press(t, m, tea.KeyEnter, 0)
	if got := m.input.Value(); got != "" {
		t.Fatalf("input %q — enter substituted a suggestion instead of running /mode", got)
	}
}

// /help opens a scrollable modal that lists every slash command.
func TestHelpScrollsAndListsAllCommands(t *testing.T) {
	m := typeKeys(t, newSized(t), "/help")
	m = press(t, m, tea.KeyEnter, 0)
	if m.state != stateHelp {
		t.Fatalf("state %v, want stateHelp", m.state)
	}
	content := m.helpContent(m.helpViewport.Width())
	for _, c := range slashCommands {
		if !strings.Contains(content, c.name) {
			t.Errorf("help omits %s", c.name)
		}
	}
	if strings.Count(content, "\n") <= m.helpViewport.Height() {
		t.Fatal("help fits without scrolling — pick a shorter test terminal")
	}
	m = press(t, m, tea.KeyDown, 0)
	if m.helpViewport.YOffset() != 1 {
		t.Fatalf("↓ left offset at %d, want 1", m.helpViewport.YOffset())
	}
	m = press(t, m, tea.KeyPgDown, 0)
	if m.helpViewport.YOffset() <= 1 {
		t.Fatalf("pgdn left offset at %d", m.helpViewport.YOffset())
	}
}
