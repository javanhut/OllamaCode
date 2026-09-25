package tui

import (
	"slices"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func selectionModel() *Model {
	m := &Model{tools: tools.DefaultRegistry(), mode: WriteMode,
		profile: ModelProfile{ParamsB: 8, MaxVisibleTools: 10}}
	m.tools.Register(m.switchModeTool())
	return m
}

func visibleNames(m *Model) []string {
	var names []string
	for _, t := range m.toolsForMode() {
		names = append(names, t.Function.Name)
	}
	return names
}

func ask(m *Model, text string) []string {
	m.history = append(m.history, api.Message{Role: "user", Content: text})
	return visibleNames(m)
}

// A new request that asks for nothing in particular keeps the previous set,
// so the tool block (and the cached prompt behind it) stays byte-identical.
func TestSelectionStaysStableAcrossUnremarkableRequests(t *testing.T) {
	m := selectionModel()
	first := ask(m, "look at the parser and tell me how tokens flow")
	second := ask(m, "and what does the lexer hand over to it")
	if !slices.Equal(first, second) {
		t.Fatalf("tool set changed without cause:\n%v\n%v", first, second)
	}
}

// A request that needs a tool the set lacks swaps it in, displacing the
// weakest, and leaves the rest alone. "Needs" is judged against a fresh
// selection: every tool the request asks for that the plain ranking would
// give a slot must end up in the sticky set too.
func TestSelectionSwapsInWhatARequestNeeds(t *testing.T) {
	m := selectionModel()
	first := ask(m, "look at the parser and tell me how tokens flow")
	if slices.Contains(first, "web_search") {
		t.Fatal("setup: web_search should not be in the first set")
	}
	query := "search the web for the latest parser documentation"
	second := ask(m, query)

	fresh := &Model{tools: m.tools, mode: m.mode, profile: m.profile}
	fresh.history = m.history[len(m.history)-1:]
	var needed []string
	for _, r := range rankTools(m.toolsForModeUncapped(), query) {
		if r.boosted && slices.Contains(visibleNames(fresh), r.tool.Function.Name) {
			needed = append(needed, r.tool.Function.Name)
		}
	}
	if !slices.Contains(needed, "web_search") {
		t.Fatalf("setup: web_search should be needed, got %v", needed)
	}
	for _, n := range needed {
		if !slices.Contains(second, n) {
			t.Fatalf("needed tool %s missing from %v", n, second)
		}
	}
	kept := 0
	for _, n := range first {
		if slices.Contains(second, n) {
			kept++
		}
	}
	if len(second) != len(first) || kept < len(first)-len(needed) {
		t.Fatalf("more churn than needed: kept %d of %d with %d needed (%v -> %v)", kept, len(first), len(needed), first, second)
	}
}

func TestSelectionIsIdempotentWithinARequest(t *testing.T) {
	m := selectionModel()
	a := ask(m, "fix the failing parser test")
	b := visibleNames(m)
	c := visibleNames(m)
	if !slices.Equal(a, b) || !slices.Equal(b, c) {
		t.Fatalf("repeated calls disagree: %v / %v / %v", a, b, c)
	}
}

func TestSelectionDropsBannedToolAndRefills(t *testing.T) {
	m := selectionModel()
	first := ask(m, "fix the failing parser test")
	victim := ""
	for _, n := range first {
		if n != "switch_mode" {
			victim = n
			break
		}
	}
	m.bannedTools = map[string]bool{victim: true}
	second := visibleNames(m)
	if slices.Contains(second, victim) || len(second) != len(first) {
		t.Fatalf("ban not applied or gap not filled: %v -> %v", first, second)
	}
}

func TestSelectionResetsOnModeChange(t *testing.T) {
	m := selectionModel()
	ask(m, "fix the failing parser test")
	m.mode = ExploreMode
	for _, n := range visibleNames(m) {
		if n == "write_file" || n == "edit_file" {
			t.Fatalf("explore mode kept a write tool from the write-mode set: %s", n)
		}
	}
}

// toolsForModeUncapped is the mode-allowed tool list before any cap.
func (m *Model) toolsForModeUncapped() []tools.Tool {
	saved := m.profile.MaxVisibleTools
	m.profile.MaxVisibleTools = 1000
	defer func() { m.profile.MaxVisibleTools = saved }()
	sel := m.selection
	defer func() { m.selection = sel }()
	return m.toolsForMode()
}
