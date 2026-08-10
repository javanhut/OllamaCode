package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

// researchTestModel builds a Model that can run a real submit: a model is
// selected and the escalation offer is pre-declined so the turn starts
// immediately instead of holding for a routing answer.
func researchTestModel(t *testing.T) *Model {
	t.Helper()
	m := newSized(t)
	m.modelName = "test-model"
	m.routeDeclines = routeMaxDeclines
	return m
}

// cancelResearchStream stops the dial-out startStream kicked off, so tests
// don't leak an HTTP attempt to a host that isn't there.
func cancelResearchStream(m *Model) {
	if m.stream != nil && m.stream.cancel != nil {
		m.stream.cancel()
	}
}

// The recipe template carries the whole flow: the question, all five phases,
// the dedupe rule, and the untrusted-content rule.
func TestResearchRecipeContent(t *testing.T) {
	recipe := researchRecipe("how do Go generics handle type sets?")
	for _, want := range []string{
		researchMarker,
		"how do Go generics handle type sets?",
		"DECOMPOSE",
		"dedupe",
		"web_search",
		"web_fetch",
		"web_crawl",
		"UNTRUSTED EXTERNAL CONTENT",
		"Sources",
		"todo_write",
	} {
		if !strings.Contains(recipe, want) {
			t.Errorf("recipe missing %q", want)
		}
	}
}

// /research <question> injects the recipe as a system message ahead of the
// user turn and seeds the checklist.
func TestResearchCommandSeedsRecipeTurnAndTodos(t *testing.T) {
	m := researchTestModel(t)
	defer cancelResearchStream(m)

	cmd := m.researchCommand("what's new in the latest Go release?")
	if cmd == nil {
		t.Fatal("expected a stream command for the research turn")
	}

	if len(m.history) != 2 {
		t.Fatalf("expected recipe + user message, got %d history entries", len(m.history))
	}
	if m.history[0].Role != "system" || !strings.HasPrefix(m.history[0].Content, researchMarker) {
		t.Fatalf("first history entry should be the recipe system message, got %+v", m.history[0])
	}
	if m.history[1].Role != "user" || m.history[1].Content != "Research: what's new in the latest Go release?" {
		t.Fatalf("second history entry should be the research question, got %+v", m.history[1])
	}

	todos := m.todos.get()
	if len(todos) != 5 {
		t.Fatalf("expected 5 seeded todos, got %d", len(todos))
	}
	if todos[0].Status != todoInProgress {
		t.Fatalf("first todo should be in_progress, got %q", todos[0].Status)
	}
	for _, it := range todos[1:] {
		if it.Status != todoPending {
			t.Fatalf("later todos should be pending, got %q for %q", it.Status, it.Content)
		}
	}
}

// With no model selected, /research explains instead of starting a turn.
func TestResearchCommandRequiresModel(t *testing.T) {
	m := newSized(t)
	m.modelName = "" // New() picks up the configured default; simulate a fresh session

	if cmd := m.researchCommand("anything"); cmd != nil {
		t.Fatal("no model: expected no stream command")
	}
	if m.toast == "" || len(m.history) != 0 {
		t.Fatalf("expected a toast and no history, got toast %q and %d entries", m.toast, len(m.history))
	}
}

// Bare /research without a research thread shows usage rather than starting
// an aimless turn.
func TestResearchCommandBareWithoutThreadShowsUsage(t *testing.T) {
	m := researchTestModel(t)

	if cmd := m.researchCommand(""); cmd != nil {
		t.Fatal("bare /research without a thread should not start a turn")
	}
	if !strings.Contains(m.toast, "usage: /research") {
		t.Fatalf("expected usage toast, got %q", m.toast)
	}
	if len(m.history) != 0 {
		t.Fatalf("history should be untouched, got %d entries", len(m.history))
	}
	if n := len(m.todos.get()); n != 0 {
		t.Fatalf("no todos should be seeded, got %d", n)
	}
}

// Bare /research on an existing thread deepens it: the follow-up nudge goes
// out as a user message and no second recipe is injected.
func TestResearchCommandBareContinuesThread(t *testing.T) {
	m := researchTestModel(t)
	m.history = append(m.history,
		api.Message{Role: "system", Content: researchRecipe("earlier question")},
		api.Message{Role: "user", Content: "Research: earlier question"},
		api.Message{Role: "assistant", Content: "First-pass answer with sources."},
	)
	defer cancelResearchStream(m)

	cmd := m.researchCommand("")
	if cmd == nil {
		t.Fatal("expected a stream command for the deepen turn")
	}

	if len(m.history) != 4 {
		t.Fatalf("expected one appended follow-up, got %d history entries", len(m.history))
	}
	last := m.history[3]
	if last.Role != "user" || last.Content != researchFollowup {
		t.Fatalf("follow-up should be the deepen nudge, got %+v", last)
	}
	recipes := 0
	for _, msg := range m.history {
		if strings.HasPrefix(msg.Content, researchMarker) {
			recipes++
		}
	}
	if recipes != 1 {
		t.Fatalf("deepen must not re-inject the recipe, found %d", recipes)
	}
}

// Typing the command end-to-end parses the args and dispatches like the other
// arg-taking slash commands.
func TestResearchSlashDispatchParsesArgs(t *testing.T) {
	m := researchTestModel(t)
	defer cancelResearchStream(m)

	m = typeKeys(t, m, "/research   spaced   question  ")
	m = press(t, m, tea.KeyEnter, 0)

	if got := m.input.Value(); got != "" {
		t.Fatalf("input should be cleared after dispatch, got %q", got)
	}
	if len(m.history) != 2 {
		t.Fatalf("expected recipe + user message, got %d entries", len(m.history))
	}
	if !strings.Contains(m.history[0].Content, "spaced   question") {
		t.Fatalf("recipe should carry the question text, got %q", m.history[0].Content)
	}
	if m.history[1].Content != "Research: spaced   question" {
		t.Fatalf("unexpected user message %q", m.history[1].Content)
	}
}
