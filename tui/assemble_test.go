package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

func msg(role, content string) api.Message { return api.Message{Role: role, Content: content} }

func TestHistoryWindow_AllFit(t *testing.T) {
	h := []api.Message{msg("user", "a"), msg("assistant", "b"), msg("user", "c")}
	if got := historyWindow(h, 1_000_000); got != 0 {
		t.Fatalf("expected all kept (start 0), got %d", got)
	}
}

func TestHistoryWindow_DropsOldest(t *testing.T) {
	// Each message ~25 tokens (100 chars). Budget fits ~2 of them.
	big := strings.Repeat("x", 100)
	h := []api.Message{msg("user", big), msg("assistant", big), msg("user", big)}
	start := historyWindow(h, 60)
	if start == 0 {
		t.Fatalf("expected oldest dropped, kept everything (start=%d)", start)
	}
	if start >= len(h) {
		t.Fatalf("must keep at least the most recent message, got start=%d", start)
	}
}

func TestHistoryWindow_KeepsAtLeastNewest(t *testing.T) {
	huge := strings.Repeat("y", 100000)
	h := []api.Message{msg("user", huge)}
	if got := historyWindow(h, 1); got != 0 {
		t.Fatalf("single oversized message must still be kept, got start=%d", got)
	}
}

func TestHistoryWindow_NoDanglingToolResult(t *testing.T) {
	// assistant(tool_call) -> tool(result) -> user. A tight budget would cut to
	// the tool message; the window must pull back to include the assistant call.
	big := strings.Repeat("z", 200)
	assistant := api.Message{Role: "assistant", ToolCalls: []mcp.ToolCall{{Function: mcp.ToolCallFunction{Name: "read_file"}}}}
	h := []api.Message{
		msg("user", big),
		assistant,
		msg("tool", big),
		msg("user", "now what"),
	}
	start := historyWindow(h, 80)
	if start < len(h) && h[start].Role == "tool" {
		t.Fatalf("window must not begin on a dangling tool result (start=%d role=%s)", start, h[start].Role)
	}
}
