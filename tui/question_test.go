package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/textarea"
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
// esc closes the picker without answering, leaving the model still waiting.
func TestQuestionEscapeLetsUserType(t *testing.T) {
	m := questionModel("yes", "no")
	m.updateQuestion(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.state != stateChat {
		t.Fatalf("esc did not return to chat: state=%v", m.state)
	}
	if len(m.history) != 0 {
		t.Fatalf("esc submitted an answer: %+v", m.history)
	}
	if m.question.Question == "" {
		t.Fatal("esc discarded the question; it should still be answerable by typing")
	}
}

func TestStageAnswerPutsChoiceWhereATypedAnswerGoes(t *testing.T) {
	m := questionModel("postgres", "sqlite")
	m.stageAnswer("sqlite")
	if m.input.Value() != "sqlite" {
		t.Fatalf("input = %q, want the chosen label", m.input.Value())
	}
	if m.state != stateChat {
		t.Fatalf("picker stayed open: state=%v", m.state)
	}
	if m.question.Question != "" || len(m.question.Options) != 0 {
		t.Fatalf("pending question was not cleared: %+v", m.question)
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
}

// Only a call that actually carries options opens the picker; an open question
// still goes through the normal typed reply.
func TestOptionlessQuestionDoesNotOpenPicker(t *testing.T) {
	if q := tools.ParseAskUser(json.RawMessage(`{"question":"What timeout?"}`)); len(q.Options) != 0 {
		t.Fatalf("unexpected options: %#v", q.Options)
	}
}
