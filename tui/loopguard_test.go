package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func tc(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestCallFingerprint_KeyOrderStable(t *testing.T) {
	a := tools.CallFingerprint(tc("edit_file", `{"path":"a","old_string":"x"}`))
	b := tools.CallFingerprint(tc("edit_file", `{"old_string":"x","path":"a"}`))
	if a != b {
		t.Fatalf("fingerprints should be key-order independent:\n%s\n%s", a, b)
	}
}

func TestCallFingerprint_DiffersByArgs(t *testing.T) {
	if tools.CallFingerprint(tc("read_file", `{"path":"a"}`)) == tools.CallFingerprint(tc("read_file", `{"path":"b"}`)) {
		t.Fatal("different args must produce different fingerprints")
	}
}

func TestBatchSingleTool(t *testing.T) {
	if got := batchSingleTool([]tools.ToolCall{tc("switch_mode", `{"mode":"plan","reason":"a"}`), tc("switch_mode", `{"mode":"plan","reason":"b"}`)}); got != "switch_mode" {
		t.Fatalf("same tool, varying args should return name, got %q", got)
	}
	if got := batchSingleTool([]tools.ToolCall{tc("read_file", `{}`), tc("grep", `{}`)}); got != "" {
		t.Fatalf("mixed tools should return empty, got %q", got)
	}
	if got := batchSingleTool(nil); got != "" {
		t.Fatalf("empty batch should return empty, got %q", got)
	}
}

func TestRepeatGuardTreatsDifferentInspectionArgumentsAsProgress(t *testing.T) {
	m := &Model{}
	for _, path := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		_, warn, stop, _ := m.observeRepeatedBatch([]tools.ToolCall{
			tc("read_file", `{"path":"`+path+`"}`),
		})
		if warn || stop {
			t.Fatalf("different read_file path %q triggered repeat guard", path)
		}
	}
	if m.sameToolStreak != 1 {
		t.Fatalf("different inspection arguments should reset streak, got %d", m.sameToolStreak)
	}
}

func TestRepeatGuardStopsExactInspectionRepeat(t *testing.T) {
	m := &Model{}
	calls := []tools.ToolCall{tc("read_file", `{"path":"same.go"}`)}
	for round := 1; round <= 6; round++ {
		_, warn, stop, announceStop := m.observeRepeatedBatch(calls)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop != (round >= 5) || announceStop != (round == 5) {
			t.Fatalf("round %d: stop=%v announceStop=%v", round, stop, announceStop)
		}
	}
}

func TestRepeatGuardWarnsOnlyOncePerTurn(t *testing.T) {
	m := &Model{failedCalls: make(map[string]int)}
	for range 3 {
		_, _, _, _ = m.observeRepeatedBatch([]tools.ToolCall{
			tc("switch_mode", `{"mode":"plan","reason":"first"}`),
		})
	}

	// A mixed batch demonstrates progress and resets the current streak.
	_, warn, _, _ := m.observeRepeatedBatch([]tools.ToolCall{
		tc("read_file", `{"path":"progress.go"}`),
		tc("grep", `{"pattern":"progress"}`),
	})
	if warn {
		t.Fatal("mixed progress batch should not warn")
	}

	for _, reason := range []string{"second-a", "second-b", "second-c"} {
		_, warn, _, _ = m.observeRepeatedBatch([]tools.ToolCall{
			tc("switch_mode", `{"mode":"plan","reason":"`+reason+`"}`),
		})
		if warn {
			t.Fatal("repeat warning emitted more than once in one user turn")
		}
	}

	m.resetTurnGuards()
	if m.sameToolWarned || m.sameToolStopWarned || m.lastStepRepeatKey != "" {
		t.Fatal("resetTurnGuards did not clear repetition state")
	}
}

func TestIsOscillating(t *testing.T) {
	if !tools.IsOscillating([]string{"A", "B", "A", "B"}) {
		t.Fatal("ABAB should be detected as oscillating")
	}
	if tools.IsOscillating([]string{"A", "A", "A", "A"}) {
		t.Fatal("AAAA is repetition, not oscillation")
	}
	if tools.IsOscillating([]string{"A", "B", "C", "D"}) {
		t.Fatal("ABCD is progress, not oscillation")
	}
	if tools.IsOscillating([]string{"A", "B"}) {
		t.Fatal("too short to oscillate")
	}
}

func TestSalvageJSON(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},                                   // already valid: unchanged
		{"```json\n{\"a\":1}\n```", `{"a":1}`},                   // fenced
		{`{"a":1,}`, `{"a":1}`},                                  // trailing comma
		{"here you go: {\"path\":\"x\"} thanks", `{"path":"x"}`}, // surrounding prose
	}
	for _, c := range cases {
		got := string(tools.SalvageJSON(json.RawMessage(c.in)))
		if !json.Valid([]byte(got)) {
			t.Errorf("tools.SalvageJSON(%q) produced invalid JSON %q", c.in, got)
			continue
		}
		if got != c.want {
			t.Errorf("tools.SalvageJSON(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSalvageJSON_UnrepairableUnchanged(t *testing.T) {
	in := json.RawMessage(`not json at all`)
	if string(tools.SalvageJSON(in)) != string(in) {
		t.Fatal("unrepairable input must be returned unchanged")
	}
}

func TestRepairHint_ValidationError(t *testing.T) {
	call := tc("edit_file", `{"path":"a"}`)
	err := tools.ValidateArgs(tools.EditFileTool().Function, call.Function.Arguments)
	if err == nil {
		t.Fatal("expected validation error")
	}
	hint := tools.RepairHint(call, err)
	if !errorsContains(hint, "new_string") {
		t.Fatalf("repair hint should name the missing field: %s", hint)
	}
}

func TestRepairHint_BrokenJSON(t *testing.T) {
	call := tc("read_file", `{"path": broken`)
	hint := tools.RepairHint(call, errors.New("invalid arguments"))
	if !errorsContains(hint, "valid JSON") {
		t.Fatalf("expected broken-JSON guidance, got: %s", hint)
	}
}

func errorsContains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRereadGuardCatchesInterleavedRereads(t *testing.T) {
	m := &Model{}
	// Read a.go, do something else, read a.go again: the streak guard resets
	// on the interleaved call, but the re-read guard must still catch it.
	if rereads, _ := m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); len(rereads) != 0 {
		t.Fatalf("first read reported as re-read: %v", rereads)
	}
	if rereads, _ := m.observeFileReads([]tools.ToolCall{tc("grep", `{"pattern":"x","path":"a.go"}`)}); len(rereads) != 0 {
		t.Fatalf("grep with same path is a different question, got rereads: %v", rereads)
	}
	rereads, _ := m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"./a.go"}`)})
	if len(rereads) != 1 || rereads[0] != "a.go" {
		t.Fatalf("re-read via ./a.go not caught or misreported: %v", rereads)
	}
}

func TestRereadGuardStopsAfterCap(t *testing.T) {
	m := &Model{}
	stop := false
	for range 5 {
		if _, stop = m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); stop {
			break
		}
	}
	if !stop {
		t.Fatal("re-read loop never hit the stop cap")
	}
	if m.rereadEvents < maxRereadEvents {
		t.Fatalf("expected at least %d reread events, got %d", maxRereadEvents, m.rereadEvents)
	}
}

func TestForgetReadsAllowsRereadAfterMutation(t *testing.T) {
	m := &Model{}
	m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)})
	m.forgetReads(tools.MutatedPaths("edit_file", json.RawMessage(`{"path":"a.go"}`)))
	if rereads, _ := m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); len(rereads) != 0 {
		t.Fatalf("re-read after mutation treated as a loop: %v", rereads)
	}
}

func TestSimilarPreamble(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Let me check how the guard behaves.", "  let   me CHECK how the guard behaves. ", true},
		{"Let me check how the guard behaves.", "Let me check how the guard behaves", true}, // containment
		{"Let me check exactly how the bypass guard behaves when .git exists",
			"Let me check exactly what the bypass guard does when .git exists", true}, // reworded echo
		{"Let me check how the guard behaves.", "The build fails because of a missing import.", false},
		{"", "anything at all", false},
	}
	for _, c := range cases {
		if got := similarPreamble(c.a, c.b); got != c.want {
			t.Errorf("similarPreamble(%q, %q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestObservePreambleWarnsOnceThenStops(t *testing.T) {
	m := &Model{}
	first := "Good question — let me check exactly how the bypass guard behaves."
	echo := "Good question — let me check exactly what the bypass guard does here."

	if warn, stop := m.observePreamble(first); warn || stop {
		t.Fatal("first preamble should not trigger anything")
	}
	if warn, _ := m.observePreamble(echo); !warn {
		t.Fatal("first echo should warn")
	}
	// second echo: no repeated warning, and no stop yet
	if warn, stop := m.observePreamble(echo); warn || stop {
		t.Fatalf("second echo: warn=%v stop=%v, want both false", warn, stop)
	}
	// third echo: the model ignored the warning — stop it
	if _, stop := m.observePreamble(echo); !stop {
		t.Fatal("persistent echoing should eventually stop tools")
	}
}

func TestObservePreambleIgnoresShortAndDistinct(t *testing.T) {
	m := &Model{}
	m.observePreamble("OK")
	if warn, _ := m.observePreamble("OK"); warn {
		t.Fatal("short acks should not count as echoes")
	}
	m.observePreamble("I'll start by mapping the repository layout and entry points.")
	if warn, _ := m.observePreamble("The build cache is stale, so I'm clearing it before rerunning tests."); warn {
		t.Fatal("distinct preambles should not warn")
	}
}

func TestResetTurnGuardsClearsNewState(t *testing.T) {
	m := &Model{failedCalls: map[string]int{}}
	m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)})
	m.observePreamble("Good question — let me check exactly how the bypass guard behaves.")
	m.observePreamble("Good question — let me check exactly how the bypass guard behaves!")
	m.resetTurnGuards()
	if len(m.turnReads) != 0 || m.rereadEvents != 0 || m.rereadStopAnnounced {
		t.Fatal("resetTurnGuards did not clear re-read state")
	}
	if m.lastPreamble != "" || m.preambleStreak != 0 || m.preambleWarned {
		t.Fatal("resetTurnGuards did not clear preamble state")
	}
}

func TestDedupeCalls(t *testing.T) {
	// Exact duplicate (key order differs) dropped; distinct args kept; order preserved.
	got := dedupeCalls([]tools.ToolCall{
		tc("edit_file", `{"path":"a","old_string":"x"}`),
		tc("read_file", `{"path":"b"}`),
		tc("edit_file", `{"old_string":"x","path":"a"}`),
		tc("read_file", `{"path":"c"}`),
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 calls after dedupe, got %d", len(got))
	}
	if got[0].Function.Name != "edit_file" || got[1].Function.Arguments[9] != 'b' || got[2].Function.Arguments[9] != 'c' {
		t.Fatalf("dedupe changed order or kept wrong calls: %+v", got)
	}
}

// TestStaleMessagesDropped: async messages from a cancelled/replaced turn (gen
// mismatch) must not touch the current turn's state — the straggler-corruption
// guard in Update.
func TestStaleMessagesDropped(t *testing.T) {
	m := &Model{mode: ExploreMode, turnGen: 2, streamBuf: &strings.Builder{}}
	m.pending = &pendingBatch{
		gen:     2,
		calls:   []tools.ToolCall{tc("read_file", `{"path":"a"}`), tc("read_file", `{"path":"b"}`)},
		results: make([]api.Message, 2),
		started: []bool{true, true},
	}

	// Stale tool result: valid index, old gen. Must not count or store.
	next, _ := m.Update(toolResultMsg{gen: 1, index: 0, result: api.Message{Role: "tool", Content: "stale"}})
	m = next.(*Model)
	if m.pending.done != 0 || m.pending.results[0].Content != "" {
		t.Fatalf("stale toolResultMsg was applied: done=%d results[0]=%q", m.pending.done, m.pending.results[0].Content)
	}

	// Stale stream chunk: must not land in the buffer.
	next, _ = m.Update(chatChunkMsg{gen: 1, content: "stale text"})
	m = next.(*Model)
	if m.streamBuf.Len() != 0 {
		t.Fatalf("stale chatChunkMsg wrote to streamBuf: %q", m.streamBuf.String())
	}

	// Stale error (e.g. "context canceled" after esc): no error surfaced, no retry.
	next, _ = m.Update(chatErrMsg{gen: 1, err: errors.New("context canceled")})
	m = next.(*Model)
	if m.lastError != "" || m.streamRetries != 0 {
		t.Fatalf("stale chatErrMsg was applied: lastError=%q retries=%d", m.lastError, m.streamRetries)
	}
}
