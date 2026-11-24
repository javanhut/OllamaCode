package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func tc(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestBatchSingleTool(t *testing.T) {
	if got := BatchSingleTool([]tools.ToolCall{tc("switch_mode", `{"mode":"plan","reason":"a"}`), tc("switch_mode", `{"mode":"plan","reason":"b"}`)}); got != "switch_mode" {
		t.Fatalf("same tool, varying args should return name, got %q", got)
	}
	if got := BatchSingleTool([]tools.ToolCall{tc("read_file", `{}`), tc("grep", `{}`)}); got != "" {
		t.Fatalf("mixed tools should return empty, got %q", got)
	}
	if got := BatchSingleTool(nil); got != "" {
		t.Fatalf("empty batch should return empty, got %q", got)
	}
}

func TestRepeatGuardTreatsDifferentInspectionArgumentsAsProgress(t *testing.T) {
	g := &LoopGuard{}
	for _, path := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		_, warn, stop, _ := g.ObserveRepeatedBatch([]tools.ToolCall{
			tc("read_file", `{"path":"`+path+`"}`),
		})
		if warn || stop {
			t.Fatalf("different read_file path %q triggered repeat guard", path)
		}
	}
	if g.SameToolStreak != 1 {
		t.Fatalf("different inspection arguments should reset streak, got %d", g.SameToolStreak)
	}
}

func TestRepeatGuardStopsExactInspectionRepeat(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("read_file", `{"path":"same.go"}`)}
	for round := 1; round <= 6; round++ {
		_, warn, stop, announceStop := g.ObserveRepeatedBatch(calls)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		// The stop announcement repeats past the threshold: latching it once left
		// a looping model with no feedback for the rest of the turn.
		if stop != (round >= 5) || announceStop != (round >= 5) {
			t.Fatalf("round %d: stop=%v announceStop=%v", round, stop, announceStop)
		}
	}
}

func TestMaterialProgressResetsSameToolGuard(t *testing.T) {
	g := &LoopGuard{}
	for round := 1; round <= 8; round++ {
		calls := []tools.ToolCall{tc("edit_file", `{"path":"file`+string(rune('a'+round))+`.go","old_string":"x","new_string":"y"}`)}
		results := []api.Message{{Role: "tool", Content: "edited file\nNew Hash: changed-" + string(rune('a'+round))}}
		progress, _, _ := g.ObserveRoundProgress(calls, results)
		if !progress {
			t.Fatalf("round %d with a new mutation was not progress", round)
		}
		_, warn, stop, _ := g.ObserveRepeatedBatch(calls, progress)
		if warn || stop {
			t.Fatalf("round %d: productive edit triggered guard (warn=%v stop=%v)", round, warn, stop)
		}
	}
}

func TestRepeatedOutcomeWarnsAtThreeAndStopsAtFive(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("edit_file", `{"path":"same.go","old_string":"x","new_string":"y"}`)}
	results := []api.Message{{Role: "tool", Content: "edited same.go\nNew Hash: unchanged"}}
	for round := 1; round <= 5; round++ {
		progress, _, _ := g.ObserveRoundProgress(calls, results)
		_, warn, stop, _ := g.ObserveRepeatedBatch(calls, progress)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop != (round >= 5) {
			t.Fatalf("round %d: stop=%v", round, stop)
		}
	}
}

func TestOutcomeOscillationWarnsThenStops(t *testing.T) {
	g := &LoopGuard{}
	sequence := []string{"A", "B", "A", "B", "A", "B"}
	for i, name := range sequence {
		calls := []tools.ToolCall{tc(name, `{}`)}
		results := []api.Message{{Role: "tool", Content: "same " + name}}
		_, warn, stop := g.ObserveRoundProgress(calls, results)
		round := i + 1
		if warn != (round == 4) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop != (round >= 6) {
			t.Fatalf("round %d: stop=%v", round, stop)
		}
	}
}

func TestChangingResultsAreNotOscillation(t *testing.T) {
	g := &LoopGuard{}
	for i, name := range []string{"A", "B", "A", "B", "A", "B"} {
		calls := []tools.ToolCall{tc(name, `{}`)}
		results := []api.Message{{Role: "tool", Content: fmt.Sprintf("new evidence %d", i)}}
		progress, warn, stop := g.ObserveRoundProgress(calls, results)
		if !progress || warn || stop {
			t.Fatalf("round %d: changing result classified as stagnant (progress=%v warn=%v stop=%v)", i+1, progress, warn, stop)
		}
	}
}

func TestStagnationUsesThreeFivePolicy(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("read_file", `{"path":"a.go"}`), tc("grep", `{"pattern":"missing"}`)}
	results := []api.Message{{Role: "tool", Content: "same file"}, {Role: "tool", Content: "same matches"}}
	for round := 1; round <= 5; round++ {
		progress, _, _ := g.ObserveRoundProgress(calls, results)
		warn, stop := g.ObserveStagnation(progress)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop != (round >= 5) {
			t.Fatalf("round %d: stop=%v", round, stop)
		}
	}
}

// The logged failure shape: one tool, identical arguments, identical result,
// which the mixed-batch-only guard ignored entirely.
func TestStagnationStopsSingleToolLoop(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("todo_write", `{"todos":[{"content":"Read ticket.md","status":"completed"}]}`)}
	results := []api.Message{{Role: "tool", Content: `{"ok":true,"summary":"todo_write completed","evidence":["todo list updated: 2/3 completed"]}`}}
	stopped := 0
	for round := 1; round <= 5; round++ {
		progress, _, _ := g.ObserveRoundProgress(calls, results)
		warn, stop := g.ObserveStagnation(progress)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop {
			stopped = round
			break
		}
	}
	if stopped != 5 {
		t.Fatalf("single-tool no-progress loop should end the turn at round 5, got %d", stopped)
	}

	g.Reset()
	if g.StagnantRounds != 0 {
		t.Fatal("Reset did not clear stagnation")
	}
}

func TestRefusedDuplicateIsNotNewProgress(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("run_shell", `{"command":"false"}`)}
	first := []api.Message{{Role: "tool", Content: "error: exit status 1"}}
	if progress, _, _ := g.ObserveRoundProgress(calls, first); !progress {
		t.Fatal("first failure is still new diagnostic evidence")
	}
	refused := []api.Message{{Role: "tool", Content: "error: you already called run_shell with these exact arguments 2 times"}}
	if progress, _, _ := g.ObserveRoundProgress(calls, refused); progress {
		t.Fatal("local duplicate refusal was incorrectly treated as new evidence")
	}
}

func TestRepeatGuardWarnsOnlyOncePerTurn(t *testing.T) {
	g := &LoopGuard{}
	for range 3 {
		_, _, _, _ = g.ObserveRepeatedBatch([]tools.ToolCall{
			tc("switch_mode", `{"mode":"plan","reason":"first"}`),
		})
	}

	// A mixed batch demonstrates progress and resets the current streak.
	_, warn, _, _ := g.ObserveRepeatedBatch([]tools.ToolCall{
		tc("read_file", `{"path":"progress.go"}`),
		tc("grep", `{"pattern":"progress"}`),
	})
	if warn {
		t.Fatal("mixed progress batch should not warn")
	}

	for _, reason := range []string{"second-a", "second-b", "second-c"} {
		_, warn, _, _ = g.ObserveRepeatedBatch([]tools.ToolCall{
			tc("switch_mode", `{"mode":"plan","reason":"`+reason+`"}`),
		})
		if warn {
			t.Fatal("repeat warning emitted more than once in one user turn")
		}
	}

	g.Reset()
	if g.sameToolWarned || g.stopWarnedTool != "" || g.lastRepeatKey != "" {
		t.Fatal("Reset did not clear repetition state")
	}
}

func TestRereadGuardCatchesInterleavedRereads(t *testing.T) {
	g := &LoopGuard{}
	// Read a.go, do something else, read a.go again: the streak guard resets
	// on the interleaved call, but the re-read guard must still catch it.
	if rereads, _ := g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); len(rereads) != 0 {
		t.Fatalf("first read reported as re-read: %v", rereads)
	}
	if rereads, _ := g.ObserveFileReads([]tools.ToolCall{tc("grep", `{"pattern":"x","path":"a.go"}`)}); len(rereads) != 0 {
		t.Fatalf("grep with same path is a different question, got rereads: %v", rereads)
	}
	rereads, _ := g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"./a.go"}`)})
	if len(rereads) != 1 || rereads[0] != "a.go" {
		t.Fatalf("re-read via ./a.go not caught or misreported: %v", rereads)
	}
}

func TestRereadGuardStopsAfterCap(t *testing.T) {
	g := &LoopGuard{}
	stop := false
	for range 5 {
		if _, stop = g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); stop {
			break
		}
	}
	if !stop {
		t.Fatal("re-read loop never hit the stop cap")
	}
	if got := g.TurnReads["read_file\x01a.go"]; got < MaxReadsPerUnchangedTarget {
		t.Fatalf("expected at least %d reads, got %d", MaxReadsPerUnchangedTarget, got)
	}
}

func TestRereadStopIsPerTarget(t *testing.T) {
	g := &LoopGuard{}
	for _, path := range []string{"a.go", "a.go", "b.go", "b.go"} {
		if _, stop := g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"`+path+`"}`)}); stop {
			t.Fatalf("two reads of %s should warn but not stop", path)
		}
	}
	if _, stop := g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); !stop {
		t.Fatal("third unchanged read of a.go should stop")
	}
}

func TestForgetReadsAllowsRereadAfterMutation(t *testing.T) {
	g := &LoopGuard{}
	g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)})
	g.ForgetReads(tools.MutatedPaths("edit_file", json.RawMessage(`{"path":"a.go"}`)))
	if rereads, _ := g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); len(rereads) != 0 {
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
	g := &LoopGuard{}
	first := "Good question — let me check exactly how the bypass guard behaves."
	echo := "Good question — let me check exactly what the bypass guard does here."

	if warn, stop := g.ObservePreamble(first); warn || stop {
		t.Fatal("first preamble should not trigger anything")
	}
	if warn, _ := g.ObservePreamble(echo); !warn {
		t.Fatal("first echo should warn")
	}
	// second echo: no repeated warning, and no stop yet
	if warn, stop := g.ObservePreamble(echo); warn || stop {
		t.Fatalf("second echo: warn=%v stop=%v, want both false", warn, stop)
	}
	// third echo: the model ignored the warning — stop it
	if _, stop := g.ObservePreamble(echo); !stop {
		t.Fatal("persistent echoing should eventually stop tools")
	}
}

func TestObservePreambleIgnoresShortAndDistinct(t *testing.T) {
	g := &LoopGuard{}
	g.ObservePreamble("OK")
	if warn, _ := g.ObservePreamble("OK"); warn {
		t.Fatal("short acks should not count as echoes")
	}
	g.ObservePreamble("I'll start by mapping the repository layout and entry points.")
	if warn, _ := g.ObservePreamble("The build cache is stale, so I'm clearing it before rerunning tests."); warn {
		t.Fatal("distinct preambles should not warn")
	}
}

func TestResetClearsAllDetectorState(t *testing.T) {
	g := &LoopGuard{}
	g.ObserveFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)})
	g.ObservePreamble("Good question — let me check exactly how the bypass guard behaves.")
	g.ObservePreamble("Good question — let me check exactly how the bypass guard behaves!")
	g.ObserveRoundProgress(
		[]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)},
		[]api.Message{{Role: "tool", Content: "contents"}},
	)
	g.StagnantRounds = 3
	g.RereadStopAnnounced = true
	g.Ban("todo_write")
	g.Reset()
	if len(g.TurnReads) != 0 || g.RereadStopAnnounced {
		t.Fatal("Reset did not clear re-read state")
	}
	if g.lastPreamble != "" || g.preambleStreak != 0 || g.preambleWarned {
		t.Fatal("Reset did not clear preamble state")
	}
	if len(g.recentOutcomes) != 0 || len(g.seenOutcomes) != 0 || g.oscillationStreak != 0 || g.StagnantRounds != 0 {
		t.Fatal("Reset did not clear progress state")
	}
	if len(g.BannedTools) != 0 {
		t.Fatal("bans must not outlive the turn")
	}
}

func TestResetDetectorClearsRepeatLatches(t *testing.T) {
	g := &LoopGuard{}
	calls := []tools.ToolCall{tc("todo_write", `{"todos":[]}`)}
	for range 5 {
		g.ObserveRepeatedBatch(calls)
	}
	g.ResetDetector(LoopStopRepeat)
	if g.SameToolStreak != 0 || g.lastRepeatKey != "" || g.stopWarnedTool != "" {
		t.Fatal("ResetDetector did not clear the repeat streak and ban latch")
	}
	// A fresh streak to the threshold must escalate again, not auto-ban.
	for range 5 {
		g.ObserveRepeatedBatch(calls)
	}
	if g.BannedTools["todo_write"] {
		t.Fatal("first crossing after a reset banned the tool")
	}
}
