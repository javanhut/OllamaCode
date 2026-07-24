package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func orderedTurnModel() *Model {
	return &Model{
		history: []api.Message{
			{Role: "assistant", Content: "FIRSTTEXT", ToolCalls: []tools.ToolCall{{
				Function: tools.ToolCallFunction{
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"a.go"}`),
				},
			}}},
			{Role: "tool", ToolName: "read_file", Content: "package main"},
			{Role: "assistant", Content: "SECONDTEXT"},
		},
		md:      newMarkdownRenderer(),
		notesMd: newMarkdownRenderer(),
	}
}

// Text interleaved with tool calls must render in chronological order —
// text, then the tool call fired at that point, then the following text —
// not all text first and all tool calls at the bottom.
func TestWriteAssistantTurn_ChronologicalOrder(t *testing.T) {
	for _, expand := range []bool{false, true} {
		m := orderedTurnModel()
		m.expandTools = expand
		m.viewport.SetWidth(80)

		turn, next := m.collectAssistantTurn(0)
		if next != len(m.history) {
			t.Fatalf("collectAssistantTurn consumed up to %d, want %d", next, len(m.history))
		}
		var b strings.Builder
		m.writeAssistantTurn(&b, &turn, false)
		out := ansi.Strip(b.String())

		iFirst := strings.Index(out, "FIRSTTEXT")
		iTool := strings.Index(out, "read_file")
		iSecond := strings.Index(out, "SECONDTEXT")
		if iFirst < 0 || iTool < 0 || iSecond < 0 {
			t.Fatalf("expand=%v: missing pieces in output:\n%s", expand, out)
		}
		if !(iFirst < iTool && iTool < iSecond) {
			t.Fatalf("expand=%v: out of order (first=%d tool=%d second=%d):\n%s",
				expand, iFirst, iTool, iSecond, out)
		}
	}
}

// Consecutive tool calls collapse into a single "▸ N tool calls" summary at
// their position in the turn.
func TestWriteAssistantTurn_CollapsedGroupsConsecutiveTools(t *testing.T) {
	m := &Model{
		history: []api.Message{
			{Role: "assistant", Content: "before", ToolCalls: []tools.ToolCall{
				{Function: tools.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}},
				{Function: tools.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{"path":"b.go"}`)}},
			}},
			{Role: "tool", ToolName: "read_file", Content: "a"},
			{Role: "tool", ToolName: "write_file", Content: "b"},
			{Role: "assistant", Content: "after"},
		},
		md:      newMarkdownRenderer(),
		notesMd: newMarkdownRenderer(),
	}
	m.viewport.SetWidth(80)

	turn, _ := m.collectAssistantTurn(0)
	var b strings.Builder
	m.writeAssistantTurn(&b, &turn, false)
	out := ansi.Strip(b.String())

	if !strings.Contains(out, "▸ 2 tool calls") {
		t.Fatalf("expected one grouped summary for 2 consecutive calls:\n%s", out)
	}
	if strings.Count(out, "ctrl+t to expand") != 1 {
		t.Fatalf("expected exactly one collapsed summary line:\n%s", out)
	}
	if !(strings.Index(out, "before") < strings.Index(out, "▸ 2 tool calls") &&
		strings.Index(out, "▸ 2 tool calls") < strings.Index(out, "after")) {
		t.Fatalf("summary not positioned between the surrounding text:\n%s", out)
	}
}
