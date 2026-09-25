package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// The summary request must be a strict continuation of the chat request the
// host last saw — same system prompt, same history messages in the same form —
// or Ollama's KV cache misses and the whole history is prefilled again.
func TestCompactionRequestIsPrefixOfChatRequest(t *testing.T) {
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 32768}
	m.history = []api.Message{
		msg("user", "fix the parser"),
		msg("assistant", "looking"),
		{Role: "system", Content: "background job finished"},
		msg("user", "and the lexer"),
		msg("assistant", "done"),
		msg("user", "thanks"),
	}
	chat := m.assembleMessages("")
	visible := m.deriveModelMessages()
	cut := compactionCut(visible)

	req := m.compactionRequest(visible[:cut], "archive_1")

	shared := req.Messages[:len(req.Messages)-1]
	if !reflect.DeepEqual(shared, chat[:len(shared)]) {
		t.Fatalf("summary request diverges from the chat prefix:\n got  %+v\n want %+v", shared, chat[:len(shared)])
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "## Next move") || !strings.Contains(last.Content, "archive_1") {
		t.Fatalf("instruction message = %+v", last)
	}
	if !reflect.DeepEqual(req.Options, m.chatOptions(false)) {
		t.Fatalf("options %v differ from the chat request's %v; a num_ctx change reloads the model", req.Options, m.chatOptions(false))
	}
}

func TestCompactionRequestMergesPriorSummary(t *testing.T) {
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 32768}
	m.archiveSummary = "PRIOR_SUMMARY_TEXT"
	req := m.compactionRequest([]api.Message{msg("user", "hi")}, "k")
	last := req.Messages[len(req.Messages)-1].Content
	if !strings.Contains(last, "PRIOR_SUMMARY_TEXT") || !strings.Contains(last, "conversation wins") {
		t.Fatalf("prior summary not merged into the instruction: %q", last)
	}
}

func TestCompactionCutKeepsToolResultsWithTheirCall(t *testing.T) {
	call := api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{}}}
	res := api.Message{Role: "tool", Content: "r"}
	visible := []api.Message{msg("user", "a"), msg("assistant", "b"), call, res, res, msg("assistant", "c")}

	cut := compactionCut(visible) // len/2 = 3 lands on a result

	if cut != 5 {
		t.Fatalf("cut = %d, want 5 (past both results of the call)", cut)
	}
	if visible[cut].Role == "tool" {
		t.Fatal("kept half starts with an orphaned tool result")
	}
}

func TestStripThinkBlock(t *testing.T) {
	cases := map[string]string{
		"<think>hmm</think>\n## Objective\nx": "## Objective\nx",
		"  ## Objective\nx  ":                 "## Objective\nx",
		"<think>unterminated":                 "<think>unterminated",
	}
	for in, want := range cases {
		if got := stripThinkBlock(in); got != want {
			t.Errorf("stripThinkBlock(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompactDoneReasonInToast(t *testing.T) {
	m := overflowTestModel(t)
	m.Update(compactDoneMsg{index: 4, reason: "summary was not shorter than the history it replaced"})
	if !strings.Contains(m.toast, "not shorter") || m.archivedThrough != 0 {
		t.Fatalf("toast = %q archivedThrough = %d", m.toast, m.archivedThrough)
	}
}
