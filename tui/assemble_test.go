package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
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
	assistant := api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "read_file"}}}}
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

func TestDedupeNudgesKeepsOnlyLatestPerTag(t *testing.T) {
	m := routedModel(nil, "small")
	m.mode = WriteMode
	m.contextLimit = 8192
	m.history = []api.Message{
		msg("user", "do the thing"),
		msg("system", "[CONTINUE] 2 todo item(s) are still open:\n- a\n- b\nKeep working."),
		msg("system", "[NO PROGRESS DETECTED] Your last three rounds returned nothing new."),
		msg("system", "A plain system notice with no tag stays put."),
		msg("system", "[CONTINUE] 1 todo item(s) are still open:\n- b\nKeep working."),
		msg("assistant", "still working"),
	}
	historyLen := len(m.history)

	out := m.assembleMessagesForTools("", nil)

	continues := 0
	latestContinue := false
	noProgress := 0
	plainNotice := false
	for _, message := range out {
		if message.Role != "system" {
			continue
		}
		if strings.HasPrefix(message.Content, "[CONTINUE]") {
			continues++
			latestContinue = strings.Contains(message.Content, "1 todo item")
		}
		if strings.HasPrefix(message.Content, "[NO PROGRESS DETECTED]") {
			noProgress++
		}
		if strings.Contains(message.Content, "plain system notice") {
			plainNotice = true
		}
	}
	if continues != 1 || !latestContinue {
		t.Fatalf("expected only the latest [CONTINUE] nudge to be sent (continues=%d latest=%v)", continues, latestContinue)
	}
	if noProgress != 1 {
		t.Fatalf("distinct nudge tags must both survive, got %d [NO PROGRESS DETECTED]", noProgress)
	}
	if !plainNotice {
		t.Fatal("non-nudge system message was filtered out")
	}
	if len(m.history) != historyLen {
		t.Fatalf("history must be untouched by the send-time filter (%d -> %d)", historyLen, len(m.history))
	}
	// The persona prompt and the dynamic tail are not nudges and must be first/last.
	if out[0].Role != "system" || !strings.Contains(out[0].Content, systemPrompt) {
		t.Fatal("persona system prompt missing from the assembled head")
	}
	if !strings.Contains(out[len(out)-1].Content, "Current mode:") {
		t.Fatal("dynamic mode tail missing from the assembled tail")
	}
}
