package tui

import (
	"encoding/json"
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

func TestAssembleMessagesToastsWhenHistoryDropped(t *testing.T) {
	// contextLimit 4096 == the generation reserve, so the whole budget goes to
	// the system prompt and the oldest messages get hard-dropped.
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 4096}
	m.history = []api.Message{msg("user", "a"), msg("assistant", "b"), msg("user", "c")}

	m.assembleMessages("")
	if !strings.Contains(m.toast, "dropped") {
		t.Fatalf("silent history drop: toast = %q", m.toast)
	}

	m.toast = ""
	m.contextLimit = 200000
	m.assembleMessages("")
	if m.toast != "" {
		t.Fatalf("nothing was dropped but toast = %q", m.toast)
	}
}

func TestObservePromptEvalIgnoresZero(t *testing.T) {
	m := &Model{}
	m.observePromptEval(500)
	m.observePromptEval(600)
	m.observePromptEval(0) // provider reported no usage — keep the last good pair
	if m.lastPromptEval != 600 || m.prevPromptEval != 500 {
		t.Fatalf("counts = (%d, %d), want (600, 500)", m.lastPromptEval, m.prevPromptEval)
	}
}

func TestShouldCompactUsesMeasuredPromptTokens(t *testing.T) {
	m := &Model{contextLimit: 10000}
	for range 6 {
		m.history = append(m.history, msg("user", "short"))
	}
	if m.shouldCompact() {
		t.Fatal("tiny history must not compact on the estimate alone")
	}
	m.lastPromptEval, m.prevPromptEval = 9000, 9000
	if !m.shouldCompact() {
		t.Fatal("the real prompt count crossed 80% and must decide")
	}
}

func TestShouldCompactIgnoresSingleOutlier(t *testing.T) {
	m := &Model{contextLimit: 10000}
	for range 6 {
		m.history = append(m.history, msg("user", "short"))
	}
	m.observePromptEval(200)
	m.observePromptEval(9000)
	if m.shouldCompact() {
		t.Fatal("one outlier turn must not fire a compaction on its own")
	}
	m.observePromptEval(9000)
	if !m.shouldCompact() {
		t.Fatal("sustained pressure must fire")
	}
}

func TestShouldCompactRequiresSixMessages(t *testing.T) {
	m := &Model{contextLimit: 10000, lastPromptEval: 99999, prevPromptEval: 99999}
	for range 5 {
		m.history = append(m.history, msg("user", "short"))
	}
	if m.shouldCompact() {
		t.Fatal("a history too short to halve must never compact")
	}
}

// toolHistory builds n rounds of assistant(tool_call) -> tool(result) with fat
// enveloped bodies, which is what pruning is meant to reclaim.
func toolHistory(n int) []api.Message {
	body := strings.Repeat("output line\n", 700)
	h := make([]api.Message, 0, n*2)
	for range n {
		h = append(h, api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "read_file"}}}})
		h = append(h, api.Message{Role: "tool", ToolName: "read_file", Content: tools.EncodeToolSuccess("read_file", body)})
	}
	return h
}

func TestPruneToolResultsStubsOnlyTheProjection(t *testing.T) {
	const rounds = 12
	m := &Model{history: toolHistory(rounds)}
	logBefore := make([]string, len(m.history))
	for i := range m.history {
		logBefore[i] = m.history[i].Content
	}

	if !m.pruneToolResults() {
		t.Fatal("prune boundary did not move")
	}

	// The RECORD keeps every byte: the transcript still shows what the tool
	// said, and a saved session still carries it.
	for i := range m.history {
		if m.history[i].Content != logBefore[i] {
			t.Fatalf("pruning rewrote the log at %d", i)
		}
	}

	// The PROJECTION is what shrinks.
	seen := 0
	visible := m.deriveModelMessages()
	for i := len(visible) - 1; i >= 0; i-- {
		if visible[i].Role != "tool" {
			continue
		}
		env, ok := tools.DecodeToolResult(visible[i].Content)
		if !ok {
			t.Fatalf("message %d stopped decoding as an envelope", i)
		}
		if seen < keepIntactToolResults {
			if len(env.Evidence) == 0 {
				t.Fatalf("newest result %d (age %d) lost its evidence", i, seen)
			}
		} else {
			if len(env.Evidence) != 0 || env.Hint != "" {
				t.Fatalf("old result %d was not pruned", i)
			}
			if !env.OK || env.Summary != "read_file completed" || !env.Truncated {
				t.Fatalf("stub lost the verdict/summary: %+v", env)
			}
		}
		seen++
	}
	if seen != rounds {
		t.Fatalf("projected %d tool results, want %d", seen, rounds)
	}
}

func TestPruneToolResultsIdempotent(t *testing.T) {
	m := &Model{history: toolHistory(12)}
	if !m.pruneToolResults() {
		t.Fatal("first pass did not move the boundary")
	}
	boundary := m.prunedThrough
	if m.pruneToolResults() {
		t.Fatal("second pass moved the boundary again with no new results")
	}
	if m.prunedThrough != boundary {
		t.Fatalf("boundary drifted: %d -> %d", boundary, m.prunedThrough)
	}
}

func TestPruneToolResultsPreservesToolPairing(t *testing.T) {
	m := &Model{history: toolHistory(12)}
	type shape struct {
		role, name string
		calls      int
	}
	before := make([]shape, len(m.history))
	for i, mm := range m.history {
		before[i] = shape{mm.Role, mm.ToolName, len(mm.ToolCalls)}
	}

	m.pruneToolResults()

	visible := m.deriveModelMessages()
	if len(visible) != len(before) {
		t.Fatalf("projection changed length: %d -> %d", len(before), len(visible))
	}
	for i, mm := range visible {
		if (shape{mm.Role, mm.ToolName, len(mm.ToolCalls)}) != before[i] {
			t.Fatalf("message %d changed shape: %+v", i, mm)
		}
	}
}

func TestCompactContextPrunesBeforeSummarizing(t *testing.T) {
	// 20 fat results are over the 80% mark; the newest 8 that survive pruning
	// are comfortably under it.
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 40000}
	m.history = toolHistory(20)
	if !m.shouldCompact() {
		t.Fatal("setup: history should already be over the threshold")
	}

	if cmd := m.compactContext(false); cmd != nil {
		t.Fatal("pruning cleared the pressure — no model round-trip should have been scheduled")
	}
	if m.compacting {
		t.Fatal("nothing async was started; compacting must stay false")
	}
	if !strings.Contains(m.toast, "pruned") {
		t.Fatalf("toast = %q, want it to mention pruning", m.toast)
	}
	if m.shouldCompact() {
		t.Fatal("pruning did not bring the estimate back under the threshold")
	}
}

func TestCompactContextSummarizesWhenNothingToPrune(t *testing.T) {
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 20000}
	prose := strings.Repeat("chat text ", 3000)
	for range 6 {
		m.history = append(m.history, msg("user", prose))
	}
	if !m.shouldCompact() {
		t.Fatal("setup: prose history should be over the threshold")
	}

	if cmd := m.compactContext(false); cmd == nil {
		t.Fatal("nothing prunable — the summarization path must still run")
	}
	if !m.compacting {
		t.Fatal("summarization path must mark the model busy compacting")
	}
}

func TestDeriveModelMessagesStripsThinking(t *testing.T) {
	const reasoning = "SECRET_REASONING_TOKENS"
	m := &Model{history: []api.Message{
		msg("user", "question"),
		{Role: "assistant", Content: "answer", Thinking: reasoning},
	}}

	for _, v := range m.deriveModelMessages() {
		if v.Thinking != "" {
			t.Fatalf("projection carried reasoning back to the model: %q", v.Thinking)
		}
	}

	// The RECORD keeps it — the transcript and /show_thinking read this.
	if m.history[1].Thinking != reasoning {
		t.Fatalf("log lost its reasoning: %q", m.history[1].Thinking)
	}

	// The property that actually matters: the wire request must not contain it.
	body, err := json.Marshal(api.ChatRequest{Model: "m", Messages: m.deriveModelMessages()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), reasoning) {
		t.Fatalf("reasoning serialized into the request: %s", body)
	}
}
