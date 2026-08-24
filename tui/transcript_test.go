package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestAskUserQuestionVisibleWhenToolCollapsed(t *testing.T) {
	for _, expand := range []bool{false, true} {
		m := &Model{
			history: []api.Message{
				{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{
					Name:      "ask_user",
					Arguments: json.RawMessage(`{"question":"What task would you like me to work on?","options":"describe task|cancel"}`),
				}}}},
				{Role: "tool", ToolName: "ask_user", Content: "QUESTION: What task would you like me to work on?"},
			},
			md:      newMarkdownRenderer(),
			notesMd: newMarkdownRenderer(),
		}
		m.expandTools = expand
		m.viewport.SetWidth(80)

		turn, _ := m.collectAssistantTurn(0)
		var b strings.Builder
		m.writeAssistantTurn(&b, &turn, false)
		out := ansi.Strip(b.String())
		if !strings.Contains(out, "What task would you like me to work on?") {
			t.Fatalf("expand=%v: question hidden with tool call:\n%s", expand, out)
		}
		if !strings.Contains(out, "describe task · cancel") {
			t.Fatalf("expand=%v: answer options not visible:\n%s", expand, out)
		}
	}
}

// A reply that arrives as transport — a JSON tool call, a <tool_call> tag — is
// machinery, not an answer. Painting it live spelled the raw envelope into the
// transcript token by token before completion could route it.
func TestStructuredStreamIsWithheldFromLiveRender(t *testing.T) {
	for _, opening := range []string{`{"name":"read_file"`, `<tool_call>{"name"`, `[{"name":"grep"`} {
		mm, _ := New().Update(tea.WindowSizeMsg{Width: 120, Height: 34})
		m := mm.(*Model)
		m.history = append(m.history, api.Message{Role: "user", Content: "read the config"})
		m.streaming = true
		m.stream = &streamState{}
		m.streamBuf.WriteString(opening)
		m.stream.visibility = true
		m.stream.hideContent = likelyStructuredOutput(m.streamBuf.String())
		if !m.stream.hideContent {
			t.Fatalf("%q was not classified as transport", opening)
		}
		m.refreshTranscript()
		if got := stripANSI(m.transcript.String()); strings.Contains(got, `"name"`) {
			t.Errorf("transport painted live for %q:\n%s", opening, got)
		}
	}

	// Prose still streams as it always did.
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	m := mm.(*Model)
	m.history = append(m.history, api.Message{Role: "user", Content: "read the config"})
	m.streaming = true
	m.stream = &streamState{}
	m.streamBuf.WriteString("Here is what LIVEPROSE the config does")
	m.stream.visibility = true
	m.stream.hideContent = likelyStructuredOutput(m.streamBuf.String())
	m.refreshTranscript()
	if !strings.Contains(stripANSI(m.transcript.String()), "LIVEPROSE") {
		t.Error("plain prose stopped rendering live")
	}
}

// The unfinished tail of a live answer renders raw while a sealed one goes
// through glamour, so the two must agree on margin, wrap width, and trailing
// blank lines — otherwise the answer shifts and the gaps around tool calls
// change the moment the turn completes.
func TestWriteAssistantTurn_StreamingMatchesSealed(t *testing.T) {
	prose := strings.Repeat("some prose about the repo and how its pieces fit together ", 6)
	for _, text := range []string{prose, prose + "\n\n"} {
		for _, width := range []int{60, 90, 120} {
			var out [2]string
			for i, streaming := range []bool{true, false} {
				m := orderedTurnModel()
				m.history[0].Content = text
				m.viewport.SetWidth(width)

				turn, _ := m.collectAssistantTurn(0)
				turn.streaming = streaming
				turn.segments[0].live = streaming
				var b strings.Builder
				m.writeAssistantTurn(&b, &turn, false)
				lines := strings.Split(ansi.Strip(b.String()), "\n")
				for j := range lines {
					lines[j] = strings.TrimRight(lines[j], " ")
				}
				out[i] = strings.Join(lines, "\n")
			}
			if out[0] != out[1] {
				t.Fatalf("width=%d text=%q streaming:\n%s\n\nsealed:\n%s", width, text, out[0], out[1])
			}
		}
	}
}

// The stable-prefix memo is keyed by width as well as content: a resize
// mid-stream rebuilds the renderer, and replaying the old memo would leave the
// finished blocks wrapped for the old width until the next block landed.
func TestStreamMarkdownRewrapsOnResize(t *testing.T) {
	m := &Model{md: newMarkdownRenderer()}
	text := strings.Repeat("some prose about the repo and how its pieces fit together ", 6) + "\n\n"

	m.viewport.SetWidth(120)
	wide := m.streamMarkdown(text)
	m.viewport.SetWidth(60)
	narrow := m.streamMarkdown(text)

	if narrow == wide {
		t.Fatal("resize did not re-render the stable prefix")
	}
	for _, line := range strings.Split(ansi.Strip(narrow), "\n") {
		if w := ansi.StringWidth(strings.TrimRight(line, " ")); w > 60 {
			t.Fatalf("line %d wide after resize to 60: %q", w, line)
		}
	}
}
