package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
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

// A long prompt is prefilled before the model can emit anything, and that
// silence is not a stalled connection: the first-token deadline has to cover
// it, or the client hangs up on a request that is making progress.
func TestPrefillBudgetCoversLongPrompts(t *testing.T) {
	if got := prefillBudget(0); got != modelStreamIdleTimeout {
		t.Fatalf("empty prompt budget %s, want the flat idle timeout %s", got, modelStreamIdleTimeout)
	}
	// The session that exposed this: 77k tokens, ~4 minutes of prefill.
	if got := prefillBudget(77000); got < 5*time.Minute {
		t.Fatalf("77k-token budget is %s — the measured prefill alone was 4m", got)
	}
}

// The status line names the wait while a big prompt is being read, so minutes
// of silence do not read as a hang.
func TestPrefillingStatus(t *testing.T) {
	m := &Model{streamBuf: &strings.Builder{}, md: newMarkdownRenderer(), notesMd: newMarkdownRenderer()}
	m.viewport.SetWidth(80)
	m.streaming = true
	m.stream = &streamState{promptTokens: 77000}

	var b strings.Builder
	m.writeAssistantTurn(&b, &assistantTurn{streaming: true, userIdx: -1}, false)
	if out := ansi.Strip(b.String()); !strings.Contains(out, "Reading 77k tokens of context") {
		t.Fatalf("no prefill status while waiting on a 77k-token prompt:\n%s", out)
	}

	m.stream.promptTokens = 500
	b.Reset()
	m.writeAssistantTurn(&b, &assistantTurn{streaming: true, userIdx: -1}, false)
	if out := ansi.Strip(b.String()); strings.Contains(out, "Reading") {
		t.Fatalf("short prompt should just say Thinking:\n%s", out)
	}
}
