package tui

import (
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// Ollama puts the token counts on the final done chunk, which arrives after the
// tool-call chunk. Returning on the tool calls alone left tool-using turns
// unmetered.
func TestDrainToolCallStreamCapturesTokenCounts(t *testing.T) {
	toolChunk := api.ChatResponse{Message: api.Message{
		ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "read_file"}}},
	}}
	ch := make(chan api.ChatResponse, 2)
	ch <- api.ChatResponse{Message: api.Message{Content: "tail"}}
	ch <- api.ChatResponse{Done: true, PromptEval: 9109, EvalCount: 206}
	close(ch)

	out := chatToolCallsMsg{calls: toolChunk.Message.ToolCalls}
	drainToolCallStream(ch, &out, toolChunk)

	if out.promptEval != 9109 || out.evalCount != 206 {
		t.Fatalf("tokens = %d/%d, want 9109/206", out.promptEval, out.evalCount)
	}
	if out.content != "tail" {
		t.Fatalf("content = %q, want the post-call text kept", out.content)
	}
	if len(out.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(out.calls))
	}
}

// A chunk that is already done must not block waiting for another.
func TestDrainToolCallStreamOnDoneChunk(t *testing.T) {
	chunk := api.ChatResponse{Done: true, PromptEval: 5, EvalCount: 7}
	out := chatToolCallsMsg{}
	drainToolCallStream(make(chan api.ChatResponse), &out, chunk)
	if out.promptEval != 5 || out.evalCount != 7 {
		t.Fatalf("tokens = %d/%d, want 5/7", out.promptEval, out.evalCount)
	}
}
