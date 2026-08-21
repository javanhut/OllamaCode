package tui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/textarea"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func questionModel(opts ...string) *Model {
	ta := textarea.New()
	return &Model{
		state:    stateQuestion,
		input:    ta,
		width:    100,
		height:   40,
		question: tools.AskUserQuestion{Question: "Which database should I use?", Options: opts},
		md:       newMarkdownRenderer(),
		notesMd:  newMarkdownRenderer(),
	}
}

// parkedQuestionModel builds the state the dispatch loop leaves behind when a
// batch parks on an ask_user call with options: the call's placeholder result
// is already in history and questionResult points at it, so a picked answer can
// rewrite the result instead of becoming a new user message.
func parkedQuestionModel(q tools.AskUserQuestion) *Model {
	m := questionModel(q.Options...)
	m.question = q
	m.history = []api.Message{
		{Role: "user", Content: "set up the database"},
		{Role: "assistant", Content: "Let me check with you first."},
		{Role: "tool", ToolName: "ask_user", Content: "QUESTION: " + q.Question + "\n\n(Stop here and wait for the user to answer before continuing.)"},
	}
	m.questionResult = len(m.history) - 1
	return m
}

func TestChooseQuestionOptionByDigit(t *testing.T) {
	options := []string{"postgres", "sqlite", "mysql"}
	if got, ok := chooseQuestionOption("2", 0, options); !ok || got != "sqlite" {
		t.Fatalf("digit picked %q ok=%v", got, ok)
	}
	// A digit naming an option this question does not have must select nothing
	// rather than submit an out-of-range choice.
	if got, ok := chooseQuestionOption("7", 0, options); ok {
		t.Fatalf("out-of-range digit selected %q", got)
	}
	if _, ok := chooseQuestionOption("3", 0, nil); ok {
		t.Fatal("a digit selected into an empty option list")
	}
}

func TestChooseQuestionOptionByCursor(t *testing.T) {
	options := []string{"postgres", "sqlite", "mysql"}
	if got, ok := chooseQuestionOption("enter", 2, options); !ok || got != "mysql" {
		t.Fatalf("enter picked %q ok=%v", got, ok)
	}
	if _, ok := chooseQuestionOption("enter", 9, options); ok {
		t.Fatal("enter selected past the end of the list")
	}
}

func TestChooseQuestionOptionIgnoresNavigation(t *testing.T) {
	for _, key := range []string{"up", "down", "j", "k", "esc", "x", "0"} {
		if got, ok := chooseQuestionOption(key, 0, []string{"a", "b"}); ok {
			t.Fatalf("key %q selected %q", key, got)
		}
	}
}

func TestQuestionCursorStopsAtEnds(t *testing.T) {
	m := questionModel("a", "b")
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.questionCursor != 0 {
		t.Fatalf("cursor went above the first option: %d", m.questionCursor)
	}
	for range 3 {
		m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.questionCursor != 1 {
		t.Fatalf("cursor ran past the last option: %d", m.questionCursor)
	}
}

// An option list that does not cover the real answer must not trap the user:
// esc closes the picker without answering, leaving the model still waiting. The
// parked tool result keeps its placeholder — the typed answer that follows is
// delivered as a user message, so the question is answered exactly once.
func TestQuestionEscapeLetsUserType(t *testing.T) {
	m := parkedQuestionModel(tools.AskUserQuestion{Question: "Proceed?", Options: []string{"yes", "no"}})
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.state != stateChat {
		t.Fatalf("esc did not return to chat: state=%v", m.state)
	}
	if len(m.history) != 3 {
		t.Fatalf("esc submitted an answer: %+v", m.history)
	}
	if !strings.HasPrefix(m.history[m.questionResult].Content, "QUESTION: ") {
		t.Fatalf("esc rewrote the parked tool result: %q", m.history[m.questionResult].Content)
	}
	if m.question.Question == "" {
		t.Fatal("esc discarded the question; it should still be answerable by typing")
	}
}

// A picked option becomes the result of the ask_user call that asked it, not a
// staged user message: history gains nothing, the input stays empty, and the
// parked placeholder is rewritten in place.
func TestApplyQuestionAnswerDeliversToolResult(t *testing.T) {
	m := parkedQuestionModel(tools.AskUserQuestion{Question: "Proceed?", Options: []string{"yes", "no"}})
	m.applyQuestionAnswer("no")
	if got := m.history[2].Content; got != "ANSWER: no" {
		t.Fatalf("tool result = %q, want %q", got, "ANSWER: no")
	}
	for _, msg := range m.history {
		if msg.Role == "user" && msg.Content != "set up the database" {
			t.Fatalf("a new user message was staged: %+v", msg)
		}
	}
	if m.input.Value() != "" {
		t.Fatalf("input was staged with %q", m.input.Value())
	}
	if m.state != stateChat {
		t.Fatalf("picker stayed open: state=%v", m.state)
	}
	if m.question.Question != "" || len(m.question.Options) != 0 || m.questionResult != -1 {
		t.Fatalf("pending question was not cleared: %+v result=%d", m.question, m.questionResult)
	}
}

// A stale questionResult must never rewrite an unrelated history entry.
func TestApplyQuestionAnswerIgnoresUnrelatedResult(t *testing.T) {
	m := questionModel("yes", "no")
	m.history = []api.Message{{Role: "tool", ToolName: "read_file", Content: "contents"}}
	m.questionResult = 0
	m.applyQuestionAnswer("yes")
	if m.history[0].Content != "contents" {
		t.Fatalf("unrelated tool result was rewritten: %q", m.history[0].Content)
	}
}

func TestOrderedQuestionOptionsPutsRecommendedFirst(t *testing.T) {
	q := tools.AskUserQuestion{Options: []string{"postgres", "sqlite", "mysql"}, Recommended: "sqlite"}
	if got, want := orderedQuestionOptions(q), []string{"sqlite", "postgres", "mysql"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered options: %#v want %#v", got, want)
	}
	// No recommendation: the model's order stands, and the slice is shared, not copied.
	q.Recommended = ""
	if got := orderedQuestionOptions(q); !reflect.DeepEqual(got, q.Options) {
		t.Fatalf("unordered options changed: %#v", got)
	}
}

// Space toggles the focused row; enter confirms the toggled labels joined with
// ", " as the ask_user tool result.
func TestMultiSelectToggleAndConfirmJoinsToolResult(t *testing.T) {
	m := parkedQuestionModel(tools.AskUserQuestion{
		Question: "Which databases apply?", Options: []string{"postgres", "sqlite", "mysql"}, MultiSelect: true,
	})
	space := tea.KeyPressMsg{Code: tea.KeySpace}
	m.updateQuestion(space)                              // toggle postgres
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyDown}) // -> sqlite
	m.updateQuestion(space)                              // toggle sqlite
	m.updateQuestion(space)                              // untoggle sqlite
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyDown}) // -> mysql
	m.updateQuestion(space)                              // toggle mysql
	if got, want := m.checkedQuestionOptions(), []string{"postgres", "mysql"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("checked options: %#v want %#v", got, want)
	}
	m.applyQuestionAnswer(strings.Join(m.multiSelectAnswer(), ", "))
	if got := m.history[2].Content; got != "ANSWER: postgres, mysql" {
		t.Fatalf("tool result = %q", got)
	}
	if m.questionChecked != nil {
		t.Fatalf("toggle state was not cleared: %v", m.questionChecked)
	}
}

// Nothing toggled: enter takes the focused row, the same default as the
// single-select picker.
func TestMultiSelectEnterDefaultsToFocusedOption(t *testing.T) {
	m := parkedQuestionModel(tools.AskUserQuestion{
		Question: "Which apply?", Options: []string{"a", "b", "c"}, MultiSelect: true,
	})
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyDown})
	if got, want := m.multiSelectAnswer(), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default answer: %#v want %#v", got, want)
	}
}

// Space means nothing in a single-select picker — it must not eat the keypress
// or conjure toggle state.
func TestSingleSelectIgnoresSpace(t *testing.T) {
	m := questionModel("yes", "no")
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeySpace})
	if m.questionChecked != nil {
		t.Fatalf("single-select question gained toggle state: %v", m.questionChecked)
	}
}

func TestQuestionModalRendersOptions(t *testing.T) {
	m := questionModel("postgres", "sqlite")
	out := m.questionModal()
	for _, want := range []string{"Which database", "1. postgres", "2. sqlite", "esc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("modal missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[ ]") || strings.Contains(out, "[x]") {
		t.Fatalf("single-select modal rendered checkboxes:\n%s", out)
	}
}

func TestQuestionModalRendersRecommendedAndCheckboxes(t *testing.T) {
	m := parkedQuestionModel(tools.AskUserQuestion{
		Question: "Which database?", Options: []string{"sqlite", "postgres"}, Recommended: "sqlite", MultiSelect: true,
	})
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeySpace})
	out := m.questionModal()
	for _, want := range []string{"[x] 1. sqlite (recommended)", "[ ] 2. postgres", "toggle", "confirm"} {
		if !strings.Contains(out, want) {
			t.Fatalf("modal missing %q:\n%s", want, out)
		}
	}
}

// Only a call that actually carries options opens the picker; an open question
// still goes through the normal typed reply.
func TestOptionlessQuestionDoesNotOpenPicker(t *testing.T) {
	if q := tools.ParseAskUser(json.RawMessage(`{"question":"What timeout?"}`)); len(q.Options) != 0 {
		t.Fatalf("unexpected options: %#v", q.Options)
	}
}
