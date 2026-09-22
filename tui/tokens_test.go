package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func resetRatio(t *testing.T) {
	t.Helper()
	SetCharsPerToken(0) // back to the default heuristic
	markRatioKey("")
	t.Cleanup(func() {
		SetCharsPerToken(0)
		markRatioKey("")
	})
}

func TestEstimateTokensDefaultRatio(t *testing.T) {
	resetRatio(t)
	if got := estimateTokens("1234"); got != 1 {
		t.Fatalf("4 chars at the default ratio should be 1 token, got %d", got)
	}
	if got := estimateTokens("12345"); got != 2 {
		t.Fatalf("5 chars should round up to 2 tokens, got %d", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("empty string should be 0 tokens, got %d", got)
	}
}

func TestEstimateTokensMeasuredRatio(t *testing.T) {
	resetRatio(t)
	SetCharsPerToken(2.0)
	if CharsPerToken() != 2.0 {
		t.Fatalf("ratio not installed: %f", CharsPerToken())
	}
	if got := estimateTokens("1234"); got != 2 {
		t.Fatalf("4 chars at 2 chars/token should be 2 tokens, got %d", got)
	}
	if got := estimateTokensLen(5); got != 3 {
		t.Fatalf("5 chars at 2 chars/token should round up to 3 tokens, got %d", got)
	}
}

func TestSetCharsPerTokenValidation(t *testing.T) {
	resetRatio(t)
	SetCharsPerToken(100) // garbage: rejected, default stays
	if CharsPerToken() != defaultCharsPerToken {
		t.Fatalf("out-of-range ratio should be rejected, got %f", CharsPerToken())
	}
	SetCharsPerToken(3.5)
	SetCharsPerToken(0) // non-positive resets to default
	if CharsPerToken() != defaultCharsPerToken {
		t.Fatalf("reset should restore the default, got %f", CharsPerToken())
	}
}

func TestEstimateMsgTokensIncludesOverheadAndToolCalls(t *testing.T) {
	resetRatio(t)
	bare := estimateMsgTokens(api.Message{Role: "user", Content: "12345678"})
	if want := 2 + 4; bare != want {
		t.Fatalf("content + overhead: got %d, want %d", bare, want)
	}
	withCall := estimateMsgTokens(api.Message{Role: "assistant", Content: "12345678", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "read", Arguments: []byte(`{"path":"main.go"}`)}}}})
	if withCall <= bare {
		t.Fatalf("tool calls should add tokens: %d <= %d", withCall, bare)
	}
}

func TestDisplayTokensIdleUsesCompletedCount(t *testing.T) {
	resetRatio(t)
	m := &Model{totalTokens: 5000, streamBuf: &strings.Builder{}}
	if got := m.displayTokens(); got != 5000 {
		t.Fatalf("idle meter should show the completed count, got %d", got)
	}
}

func TestDisplayTokensMidTurnShowsLiveEstimate(t *testing.T) {
	resetRatio(t)
	m := &Model{
		streaming:   true,
		totalTokens: 100, // stale: last completed turn
		streamBuf:   &strings.Builder{},
		history:     []api.Message{{Role: "user", Content: strings.Repeat("x", 4000)}},
	}
	m.streamBuf.WriteString(strings.Repeat("y", 2000))
	// Estimate: 4000/4 + 4 for the history message, 2000/4 for the partial
	// reply — far ahead of the stale 100.
	if got := m.displayTokens(); got != 1004+500 {
		t.Fatalf("mid-turn meter should show the live estimate, got %d", got)
	}
}

func TestDisplayTokensNeverDropsBelowCompletedCount(t *testing.T) {
	resetRatio(t)
	m := &Model{
		streaming:   true,
		totalTokens: 50000,
		streamBuf:   &strings.Builder{},
		history:     []api.Message{{Role: "user", Content: "hi"}},
	}
	if got := m.displayTokens(); got != 50000 {
		t.Fatalf("meter should never drop below the completed count, got %d", got)
	}
}

func TestDisplayTokensNilStreamBuf(t *testing.T) {
	resetRatio(t)
	m := &Model{streaming: true, totalTokens: 42}
	if got := m.displayTokens(); got != 42 {
		t.Fatalf("nil streamBuf should fall back to the completed count, got %d", got)
	}
}
