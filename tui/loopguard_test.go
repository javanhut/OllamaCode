package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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
		// The stop announcement repeats past the threshold: latching it once left
		// a looping model with no feedback for the rest of the turn.
		if stop != (round >= 5) || announceStop != (round >= 5) {
			t.Fatalf("round %d: stop=%v announceStop=%v", round, stop, announceStop)
		}
	}
}

func TestMaterialProgressResetsSameToolGuard(t *testing.T) {
	m := &Model{}
	for round := 1; round <= 8; round++ {
		calls := []tools.ToolCall{tc("edit_file", `{"path":"file`+string(rune('a'+round))+`.go","old_string":"x","new_string":"y"}`)}
		results := []api.Message{{Role: "tool", Content: "edited file\nNew Hash: changed-" + string(rune('a'+round))}}
		progress, _, _ := m.observeRoundProgress(calls, results)
		if !progress {
			t.Fatalf("round %d with a new mutation was not progress", round)
		}
		_, warn, stop, _ := m.observeRepeatedBatch(calls, progress)
		if warn || stop {
			t.Fatalf("round %d: productive edit triggered guard (warn=%v stop=%v)", round, warn, stop)
		}
	}
}

// The ocode.log failure: 33 todo_write calls in one turn. Each reworded item
// hashed as new evidence, so the repeat streak reset forever and the tool was
// never taken away.
func TestBookkeepingChurnCannotResetRepeatGuardAndTheToolGetsBanned(t *testing.T) {
	m := &Model{mode: ExploreMode, tools: baseRegistry(&sessionNotes{}, &todoList{}, nil), failedCalls: map[string]int{}}
	m.resetTurnGuards() // every real turn starts here; the ban map is owned by it
	calls := []tools.ToolCall{tc("todo_write", `{"todos":[{"content":"Read ticket.md","status":"pending"}]}`)}
	for round := 1; round <= 6; round++ {
		// madeProgress=true every round: a reworded todo item hashes differently.
		_, _, stop, announceStop := m.observeRepeatedBatch(calls, true)
		if stop != (round >= 5) || announceStop != (round >= 5) {
			t.Fatalf("round %d: stop=%v announceStop=%v streak=%d", round, stop, announceStop, m.sameToolStreak)
		}
	}
	if !m.bannedTools["todo_write"] {
		t.Fatal("todo_write kept looping past the hard stop and was never banned")
	}
	for _, tool := range m.toolsForMode() {
		if tool.Function.Name == "todo_write" {
			t.Fatal("banned tool is still offered in the schema")
		}
	}

	// switch_mode is the only way out of a read-only mode; banning it would
	// strand the turn with no route to write mode.
	for range 8 {
		_, _, _, _ = m.observeRepeatedBatch([]tools.ToolCall{tc("switch_mode", `{"mode":"write","reason":"go"}`)}, true)
	}
	if m.bannedTools["switch_mode"] {
		t.Fatal("switch_mode must never be banned")
	}

	m.resetTurnGuards()
	if len(m.bannedTools) != 0 {
		t.Fatal("bans must not outlive the user turn")
	}
}

// Withdrawing a tool from the schema does not hold the ban: the text-form
// fallback parses calls out of assistant prose, so dispatch is the choke point
// that has to refuse them.
func TestBannedToolIsRefusedInDispatch(t *testing.T) {
	m := interruptTestModel()
	m.todos = &todoList{}
	m.bannedTools = map[string]bool{"read_file": true}
	call := tc("read_file", `{"path":"tui/loopguard.go"}`)
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}
	m.processPendingTools()
	if m.stream != nil {
		m.stream.cancel()
	}
	if m.state == statePermission {
		t.Fatal("a banned tool reached the permission prompt")
	}
	var result api.Message
	for _, msg := range m.history {
		if msg.Role == "tool" {
			result = msg
		}
	}
	if !strings.Contains(result.Content, "disabled for the rest of this turn") {
		t.Fatalf("banned call was not refused: %#v", result)
	}
	if m.failedCalls[tools.CallFingerprint(call)] == 0 {
		t.Fatal("a refused call must count as a failure so the repeat short-circuit sees it")
	}
}

// Guard advisories are turn messages, not mid-history system messages: many
// open-weight chat templates only honor a leading system message, and the
// advisory that breaks a loop is the one that must not be dropped.
func TestGuardAdvisoriesAreUserTurnsAfterResults(t *testing.T) {
	m := interruptTestModel()
	m.todos = &todoList{}
	m.profile.SupportsTools = true
	m.history = []api.Message{{Role: "user", Content: "find the bug"}}
	// Two reads of a.go already happened this turn, so this batch's third read
	// trips both the re-read advisory and its stop.
	m.turnReads = map[string]int{"read_file\x01a.go": 2}
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{tc("read_file", `{"path":"a.go"}`)},
		results: []api.Message{{Role: "tool", ToolName: "read_file", Content: "contents"}},
		started: []bool{true},
		done:    1,
	}

	m.processPendingTools()
	if m.stream != nil {
		m.stream.cancel()
	}

	lastTool, firstAdvisory := -1, -1
	var advisories []api.Message
	for i, msg := range m.history {
		switch {
		case msg.Role == "tool":
			lastTool = i
		case msg.Advisory:
			if firstAdvisory < 0 {
				firstAdvisory = i
			}
			advisories = append(advisories, msg)
		}
	}
	if lastTool < 0 {
		t.Fatalf("batch results never reached history: %+v", m.history)
	}
	if firstAdvisory < lastTool {
		t.Fatalf("advisory at %d splices into the batch's results (last result at %d)", firstAdvisory, lastTool)
	}
	want := []string{
		`[RE-READ DETECTED] You already read "a.go" this turn and nothing has changed it since — you have the contents. Use them, or grep for the specific thing you need instead of re-reading whole files.`,
		"[LOOP BROKEN] You keep re-reading files you already have the contents of. Tools are disabled for your next message — answer the user in plain text with what you know.",
	}
	if len(advisories) != len(want) {
		t.Fatalf("expected %d advisories, got %+v", len(want), advisories)
	}
	for i, msg := range advisories {
		if msg.Content != want[i] {
			t.Errorf("advisory %d text changed:\n got %q\nwant %q", i, msg.Content, want[i])
		}
		if msg.Role != "user" {
			t.Errorf("advisory %d has role %q, want user", i, msg.Role)
		}
		if isUserTurn(msg) {
			t.Errorf("advisory %d reads as something the human typed", i)
		}
	}
	// The stop is only real if the next request actually goes out tool-less.
	if m.stream == nil || !m.stream.toolsSuppressed {
		t.Fatal("re-read stop did not suppress tools for the next message")
	}
}

// The role flip's obvious failure mode: a scan for "the last thing the user
// asked for" stopping on the guards' own output.
func TestAdvisoryIsNotTheUsersLastMessage(t *testing.T) {
	m := &Model{history: []api.Message{
		{Role: "user", Content: "Fix the parser"},
		{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}},
		{Role: "tool", ToolName: "read_file", Content: "contents"},
		advisory("[LOOP BROKEN] You keep re-reading files you already have."),
	}}
	if got := m.latestUserRequest(); got != "fix the parser" {
		t.Fatalf("tool-relevance query picked up an advisory: %q", got)
	}

	// The flag, not the text, is what separates the two — a human is free to
	// open their message with "[LOOP BROKEN]" and still own the turn.
	cases := []struct {
		name string
		msg  api.Message
		want bool
	}{
		{"advisory", advisory("[LOOP BROKEN] stop"), false},
		{"advisory with em-dash tag", advisory("[TURN ENDED — NO PROGRESS] done"), false},
		{"human quoting an advisory tag", api.Message{Role: "user", Content: "[LOOP BROKEN] stop"}, true},
		{"plain request", api.Message{Role: "user", Content: "fix the parser"}, true},
		{"assistant", api.Message{Role: "assistant", Content: "hello"}, false},
		{"tool result", api.Message{Role: "tool", Content: "contents"}, false},
	}
	for _, c := range cases {
		if got := isUserTurn(c.msg); got != c.want {
			t.Errorf("%s: isUserTurn=%v want %v", c.name, got, c.want)
		}
	}
}

func TestRepeatedOutcomeWarnsAtThreeAndStopsAtFive(t *testing.T) {
	m := &Model{}
	calls := []tools.ToolCall{tc("edit_file", `{"path":"same.go","old_string":"x","new_string":"y"}`)}
	results := []api.Message{{Role: "tool", Content: "edited same.go\nNew Hash: unchanged"}}
	for round := 1; round <= 5; round++ {
		progress, _, _ := m.observeRoundProgress(calls, results)
		_, warn, stop, _ := m.observeRepeatedBatch(calls, progress)
		if warn != (round == 3) {
			t.Fatalf("round %d: warn=%v", round, warn)
		}
		if stop != (round >= 5) {
			t.Fatalf("round %d: stop=%v", round, stop)
		}
	}
}

func TestOutcomeOscillationWarnsThenStops(t *testing.T) {
	m := &Model{}
	sequence := []string{"A", "B", "A", "B", "A", "B"}
	for i, name := range sequence {
		calls := []tools.ToolCall{tc(name, `{}`)}
		results := []api.Message{{Role: "tool", Content: "same " + name}}
		_, warn, stop := m.observeRoundProgress(calls, results)
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
	m := &Model{}
	for i, name := range []string{"A", "B", "A", "B", "A", "B"} {
		calls := []tools.ToolCall{tc(name, `{}`)}
		results := []api.Message{{Role: "tool", Content: fmt.Sprintf("new evidence %d", i)}}
		progress, warn, stop := m.observeRoundProgress(calls, results)
		if !progress || warn || stop {
			t.Fatalf("round %d: changing result classified as stagnant (progress=%v warn=%v stop=%v)", i+1, progress, warn, stop)
		}
	}
}

func TestStagnationUsesThreeFivePolicy(t *testing.T) {
	m := &Model{}
	calls := []tools.ToolCall{tc("read_file", `{"path":"a.go"}`), tc("grep", `{"pattern":"missing"}`)}
	results := []api.Message{{Role: "tool", Content: "same file"}, {Role: "tool", Content: "same matches"}}
	for round := 1; round <= 5; round++ {
		progress, _, _ := m.observeRoundProgress(calls, results)
		warn, stop := m.observeStagnation(progress)
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
	m := &Model{}
	calls := []tools.ToolCall{tc("todo_write", `{"todos":[{"content":"Read ticket.md","status":"completed"}]}`)}
	results := []api.Message{{Role: "tool", Content: `{"ok":true,"summary":"todo_write completed","evidence":["todo list updated: 2/3 completed"]}`}}
	stopped := 0
	for round := 1; round <= 5; round++ {
		progress, _, _ := m.observeRoundProgress(calls, results)
		warn, stop := m.observeStagnation(progress)
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

	m.endTurnAfterReply = true
	m.resetTurnGuards()
	if m.endTurnAfterReply || m.stagnantRounds != 0 {
		t.Fatal("resetTurnGuards did not clear the forced turn end")
	}
}

// The forced ending is only real if it closes the two paths that re-invoke the
// model after a final answer: the open-todo [CONTINUE] nudge and the explore
// citation gate. Each one alone kept the logged turn alive for another round.
func TestStagnationEndingBlocksTurnReentry(t *testing.T) {
	m := &Model{mode: ExploreMode, maxSteps: defaultMaxSteps, todos: &todoList{}}
	m.todos.set([]todoItem{{Content: "still open", Status: todoInProgress}})
	m.endTurnAfterReply = true

	// The answer really is gate-worthy, so the nil below means the stop fired
	// and not that the gate had nothing to complain about.
	const answer = "The fix is in tui/definitely_missing_file.go:9000."
	if len(citationProblems(workspaceRoot(), answer)) == 0 {
		t.Fatal("test answer no longer trips the citation gate")
	}
	if cmd := m.maybeCitationGate(answer); cmd != nil {
		t.Fatal("citation gate re-invoked the model on a turn stopped for stagnation")
	}
	if len(m.history) != 0 {
		t.Fatalf("stopped turn still got a corrective message: %+v", m.history)
	}

	// The other door: open todos normally earn a [CONTINUE] nudge, and the
	// stalled model's open todos are exactly what it was churning.
	full := &Model{
		mode: WriteMode, turnGen: 1, maxSteps: defaultMaxSteps,
		tools: tools.NewRegistry(), notes: &sessionNotes{}, todos: &todoList{},
		failedCalls: map[string]int{},
		transcript:  &strings.Builder{}, streamBuf: &strings.Builder{},
		md: newMarkdownRenderer(), notesMd: newMarkdownRenderer(),
	}
	full.todos.set([]todoItem{{Content: "still open", Status: todoInProgress}})
	full.viewport.SetWidth(80)
	full.endTurnAfterReply = true
	next, _ := full.Update(chatDoneMsg{gen: 1, content: "I made no progress; here is what I know."})
	after := next.(*Model)
	if after.autoContinues != 0 {
		t.Fatalf("stopped turn was auto-continued %d time(s)", after.autoContinues)
	}
	for _, msg := range after.history {
		if strings.Contains(msg.Content, "[CONTINUE]") {
			t.Fatal("stopped turn was nudged back into its open todos")
		}
	}
}

func TestRefusedDuplicateIsNotNewProgress(t *testing.T) {
	m := &Model{}
	calls := []tools.ToolCall{tc("run_shell", `{"command":"false"}`)}
	first := []api.Message{{Role: "tool", Content: "error: exit status 1"}}
	if progress, _, _ := m.observeRoundProgress(calls, first); !progress {
		t.Fatal("first failure is still new diagnostic evidence")
	}
	refused := []api.Message{{Role: "tool", Content: "error: you already called run_shell with these exact arguments 2 times"}}
	if progress, _, _ := m.observeRoundProgress(calls, refused); progress {
		t.Fatal("local duplicate refusal was incorrectly treated as new evidence")
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
	if m.sameToolWarned || m.stopWarnedTool != "" || m.lastStepRepeatKey != "" {
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
	if got := m.turnReads["read_file\x01a.go"]; got < maxReadsPerUnchangedTarget {
		t.Fatalf("expected at least %d reads, got %d", maxReadsPerUnchangedTarget, got)
	}
}

func TestRereadStopIsPerTarget(t *testing.T) {
	m := &Model{}
	for _, path := range []string{"a.go", "a.go", "b.go", "b.go"} {
		if _, stop := m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"`+path+`"}`)}); stop {
			t.Fatalf("two reads of %s should warn but not stop", path)
		}
	}
	if _, stop := m.observeFileReads([]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}); !stop {
		t.Fatal("third unchanged read of a.go should stop")
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
	m.observeRoundProgress(
		[]tools.ToolCall{tc("read_file", `{"path":"a.go"}`)},
		[]api.Message{{Role: "tool", Content: "contents"}},
	)
	m.stagnantRounds = 3
	m.loopEscalation = &loopEscalation{kind: loopStopStagnation, rounds: 3}
	m.loopContinues = map[string]int{loopStopRepeat: 2}
	m.resetTurnGuards()
	if len(m.turnReads) != 0 || m.rereadEvents != 0 || m.rereadStopAnnounced {
		t.Fatal("resetTurnGuards did not clear re-read state")
	}
	if m.lastPreamble != "" || m.preambleStreak != 0 || m.preambleWarned {
		t.Fatal("resetTurnGuards did not clear preamble state")
	}
	if len(m.recentOutcomes) != 0 || len(m.seenOutcomes) != 0 || m.oscillationStreak != 0 || m.stagnantRounds != 0 {
		t.Fatal("resetTurnGuards did not clear progress state")
	}
	if m.loopEscalation != nil || len(m.loopContinues) != 0 {
		t.Fatal("resetTurnGuards did not clear loop-escalation state")
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

// TestSuppressedRequestDropsTextToolCall: when a request went out with its
// tools withheld, a tool call parsed back out of the reply's text must not run,
// and its JSON must never reach the transcript as the assistant's answer. This
// is what made "[LOOP BROKEN] tools are disabled" decorative.
func TestSuppressedRequestDropsTextToolCall(t *testing.T) {
	model := func(suppressed bool) *Model {
		registry := tools.NewRegistry()
		registry.Register(tools.Tool{Function: tools.Function{Name: "todo_write",
			Parameters: tools.Schema{Type: "object"}}})
		m := &Model{
			mode: ExploreMode, turnGen: 1, maxSteps: defaultMaxSteps,
			tools: registry, notes: &sessionNotes{}, failedCalls: map[string]int{},
			transcript: &strings.Builder{}, streamBuf: &strings.Builder{},
			md: newMarkdownRenderer(), notesMd: newMarkdownRenderer(),
			stream: &streamState{gen: 1, toolsSuppressed: suppressed},
		}
		m.viewport.SetWidth(80)
		return m
	}
	const call = `{"name":"todo_write","arguments":{"todos":[]}}`

	next, _ := model(true).Update(chatDoneMsg{gen: 1, content: call})
	m := next.(*Model)
	if m.pending != nil {
		t.Fatal("a tool call from a deliberately tool-less request was executed")
	}
	for _, msg := range m.history {
		if msg.Role == "assistant" && strings.Contains(msg.Content, "todo_write") {
			t.Fatalf("raw tool-call JSON surfaced as the answer: %q", msg.Content)
		}
	}
	last := m.history[len(m.history)-1]
	if !last.Advisory || !strings.Contains(last.Content, "NOT executed") {
		t.Fatalf("model was not told its call was dropped: %+v", last)
	}

	// An answer with a self-check call appended is the common shape, and the gate
	// that suppressed tools re-invoked precisely to GET that answer. Drop the
	// call, keep the prose.
	next, _ = model(true).Update(chatDoneMsg{gen: 1, content: "Done — nothing further is needed.\n<tool_call>" + call + "</tool_call>"})
	m = next.(*Model)
	if m.pending != nil {
		t.Fatal("a tool call from a deliberately tool-less request was executed")
	}
	kept := false
	for _, msg := range m.history {
		if msg.Role == "assistant" && strings.Contains(msg.Content, "Done — nothing further is needed.") && !strings.Contains(msg.Content, "todo_write") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the answer was discarded along with the dropped call: %+v", m.history)
	}

	// The native channel is the other door into the same execution path: a
	// provider that returns tool_calls on a request that carried none must not
	// get them run either.
	next, _ = model(true).Update(chatToolCallsMsg{gen: 1, content: "No changes needed.",
		calls: []tools.ToolCall{tc("todo_write", `{"todos":[]}`)}})
	m = next.(*Model)
	if m.pending != nil {
		t.Fatal("a native tool call from a deliberately tool-less request was executed")
	}

	// Control: the same reply on an unsuppressed request still executes, so the
	// test fails for the right reason and not because the parser stopped working.
	next, _ = model(false).Update(chatDoneMsg{gen: 1, content: call})
	if next.(*Model).pending == nil {
		t.Fatal("text-form tool calls must still run when tools were not withheld")
	}
}

// Doom-loop escalation: a detector crossing its stop threshold parks the turn
// on a user choice instead of ending it. These tests drive the real dispatch
// path with an already-finished batch, the same way the tool-result handler
// does.

// seedRepeatRounds drives identical todo_write rounds through the guards
// directly. Each round returns fresh text, so the stagnation guard sees
// progress and stays out of the way — only the repeat streak is armed.
// todo_write is bookkeeping, so its own output cannot reset the streak.
func seedRepeatRounds(m *Model, call tools.ToolCall, rounds ...int) {
	for _, i := range rounds {
		results := []api.Message{{Role: "tool", ToolName: "todo_write", Content: fmt.Sprintf("todo list updated: %d", i)}}
		progress, _, _ := m.observeRoundProgress([]tools.ToolCall{call}, results)
		m.observeStagnation(progress)
		m.observeRepeatedBatch([]tools.ToolCall{call}, progress)
	}
}

// repeatLoopTestModel returns a model four identical rounds into a turn — one
// round short of the repeat guard's stop threshold.
func repeatLoopTestModel(t *testing.T) (*Model, tools.ToolCall) {
	t.Helper()
	m := interruptTestModel()
	m.todos = &todoList{}
	m.profile.SupportsTools = true
	m.resetTurnGuards()
	call := tc("todo_write", `{"todos":[{"content":"Read ticket.md","status":"pending"}]}`)
	seedRepeatRounds(m, call, 1, 2, 3, 4)
	return m, call
}

// dispatchRepeatRound runs one more identical batch through the real dispatch
// path — the round that crosses the stop threshold.
func dispatchRepeatRound(m *Model, call tools.ToolCall, n int) {
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: []api.Message{{Role: "tool", ToolName: "todo_write", Content: fmt.Sprintf("todo list updated: %d", n)}},
		started: []bool{true},
		done:    1,
	}
	m.processPendingTools()
}

func cancelStream(m *Model) {
	if m.stream != nil {
		m.stream.cancel()
		m.stream = nil
	}
}

func TestRepeatStopEscalatesToModalInsteadOfEndingTurn(t *testing.T) {
	m, call := repeatLoopTestModel(t)
	dispatchRepeatRound(m, call, 5)

	if m.state != stateLoopGuard {
		t.Fatalf("repeat stop should open the escalation modal, state=%v", m.state)
	}
	esc := m.loopEscalation
	if esc == nil || esc.kind != loopStopRepeat || esc.tool != "todo_write" {
		t.Fatalf("parked escalation = %+v, want repeat on todo_write", esc)
	}
	if esc.reason() != "repeated identical todo_write calls 5 times" {
		t.Fatalf("reason = %q", esc.reason())
	}
	if options := loopEscalationOptions(esc); len(options) != 3 || options[2] != "Ban todo_write & continue" {
		t.Fatalf("a single-tool loop must offer the ban choice, got %v", options)
	}
	// The pause is genuine: nothing stopped, nothing streamed, the turn waits.
	if m.turnStoppedByGuard || m.suppressToolsOnce {
		t.Fatal("turn stopped before the user chose")
	}
	if m.stream != nil {
		t.Fatal("stream started while the modal was open")
	}
}

func TestOscillationStopEscalatesToModal(t *testing.T) {
	m := interruptTestModel()
	m.todos = &todoList{}
	m.profile.SupportsTools = true
	m.resetTurnGuards()
	for _, name := range []string{"A", "B", "A", "B", "A"} {
		calls := []tools.ToolCall{tc(name, `{}`)}
		results := []api.Message{{Role: "tool", ToolName: name, Content: "same " + name}}
		progress, _, _ := m.observeRoundProgress(calls, results)
		m.observeStagnation(progress)
		m.observeRepeatedBatch(calls, progress)
	}
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{tc("B", `{}`)},
		results: []api.Message{{Role: "tool", ToolName: "B", Content: "same B"}},
		started: []bool{true},
		done:    1,
	}
	m.processPendingTools()

	if m.state != stateLoopGuard {
		t.Fatalf("oscillation stop should open the escalation modal, state=%v", m.state)
	}
	esc := m.loopEscalation
	if esc == nil || esc.kind != loopStopOscillation {
		t.Fatalf("parked escalation = %+v, want oscillation", esc)
	}
	// A/B/A/B names no single culprit, so there is nothing to ban.
	if esc.tool != "" || len(loopEscalationOptions(esc)) != 2 {
		t.Fatalf("oscillation must not offer a ban choice, options=%v", loopEscalationOptions(esc))
	}
	// A pure A/B loop's outcomes are all repeats, so the stagnation stop
	// co-fired in the same round — the parked stop must carry it, or "stop
	// turn" could not reproduce the automatic path.
	if !esc.stopStagnant {
		t.Fatal("co-fired stagnation stop was dropped from the parked escalation")
	}
}

func TestStagnationStopEscalatesToModal(t *testing.T) {
	m := interruptTestModel()
	m.todos = &todoList{}
	m.profile.SupportsTools = true
	m.resetTurnGuards()
	calls := []tools.ToolCall{tc("read_file", `{"path":"a.go"}`), tc("grep", `{"pattern":"missing"}`)}
	results := []api.Message{{Role: "tool", ToolName: "read_file", Content: "same file"}, {Role: "tool", ToolName: "grep", Content: "same matches"}}
	for range 4 {
		progress, _, _ := m.observeRoundProgress(calls, results)
		m.observeStagnation(progress)
		m.observeRepeatedBatch(calls, progress)
	}
	m.pending = &pendingBatch{calls: calls, results: results, started: []bool{true, true}, done: 2}
	m.processPendingTools()

	if m.state != stateLoopGuard {
		t.Fatalf("stagnation stop should open the escalation modal, state=%v", m.state)
	}
	esc := m.loopEscalation
	if esc == nil || esc.kind != loopStopStagnation {
		t.Fatalf("parked escalation = %+v, want stagnation", esc)
	}
	if esc.reason() != "no progress for 4 rounds" {
		t.Fatalf("reason = %q", esc.reason())
	}
	if len(loopEscalationOptions(esc)) != 2 {
		t.Fatalf("a mixed-batch stall has no single tool to ban, options=%v", loopEscalationOptions(esc))
	}
}

// "Stop turn" must be indistinguishable from the automatic stop the guards
// always performed — same advisories, same flags, same tool-less reply.
func TestLoopEscalationStopTurnMatchesAutomaticStop(t *testing.T) {
	asked, call := repeatLoopTestModel(t)
	dispatchRepeatRound(asked, call, 5)
	if asked.state != stateLoopGuard {
		t.Fatalf("expected the escalation modal, state=%v", asked.state)
	}

	auto, autoCall := repeatLoopTestModel(t)
	auto.mode = AutoMode
	dispatchRepeatRound(auto, autoCall, 5)
	cancelStream(auto)
	if auto.state == stateLoopGuard || auto.loopEscalation != nil {
		t.Fatal("auto mode runs unattended — it must never open the modal")
	}

	next, _ := asked.updateLoopGuard(tea.KeyPressMsg{Code: tea.KeyEnter})
	asked = next.(*Model)
	defer cancelStream(asked)

	if asked.turnStoppedByGuard != auto.turnStoppedByGuard || asked.endTurnAfterReply != auto.endTurnAfterReply {
		t.Fatal("'stop turn' diverged from the automatic stop's flags")
	}
	advisories := func(m *Model) []string {
		var out []string
		for _, msg := range m.history {
			if msg.Advisory {
				out = append(out, msg.Content)
			}
		}
		return out
	}
	if got, want := advisories(asked), advisories(auto); !slices.Equal(got, want) {
		t.Fatalf("'stop turn' advisories diverged:\n got %q\nwant %q", got, want)
	}
	if asked.stream == nil || !asked.stream.toolsSuppressed {
		t.Fatal("'stop turn' must stream the blocker report tool-less")
	}
}

func TestLoopEscalationContinueResetsOnlyThatDetector(t *testing.T) {
	m, call := repeatLoopTestModel(t)
	dispatchRepeatRound(m, call, 5)
	if m.loopContinues[loopStopRepeat] != 0 {
		t.Fatal("no continue should be recorded before the user makes one")
	}

	next, _ := m.updateLoopGuard(tea.KeyPressMsg{Code: tea.KeyDown})
	m = next.(*Model)
	if m.loopGuardCursor != 1 {
		t.Fatalf("cursor = %d, want 1 (Continue anyway)", m.loopGuardCursor)
	}
	next, _ = m.updateLoopGuard(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(*Model)
	defer cancelStream(m)

	if m.state != stateChat {
		t.Fatalf("modal did not close, state=%v", m.state)
	}
	if m.sameToolStreak != 0 || m.lastStepRepeatKey != "" || m.stopWarnedTool != "" {
		t.Fatal("continue did not reset the repeat detector")
	}
	if m.loopContinues[loopStopRepeat] != 1 {
		t.Fatalf("continue grant not recorded: %v", m.loopContinues)
	}
	if m.turnStoppedByGuard || m.endTurnAfterReply {
		t.Fatal("continue stopped the turn")
	}
	if len(m.bannedTools) != 0 {
		t.Fatal("continue banned a tool")
	}
	if m.stream == nil || m.stream.toolsSuppressed {
		t.Fatal("continue must resume a normal tool-carrying stream")
	}
	for _, msg := range m.history {
		if msg.Advisory && strings.Contains(msg.Content, "LOOP BROKEN") {
			t.Fatal("continue still told the model the loop was broken")
		}
	}
}

func TestLoopEscalationBanToolAndContinue(t *testing.T) {
	m, call := repeatLoopTestModel(t)
	dispatchRepeatRound(m, call, 5)

	next, _ := m.updateLoopGuard(tea.KeyPressMsg{Code: '3', Text: "3"})
	m = next.(*Model)
	defer cancelStream(m)

	if !m.bannedTools["todo_write"] {
		t.Fatal("ban choice did not withdraw todo_write for the rest of the turn")
	}
	if m.sameToolStreak != 0 || m.stopWarnedTool != "" {
		t.Fatal("ban did not reset the repeat detector")
	}
	if m.loopContinues[loopStopRepeat] != 0 {
		t.Fatal("banning is a resolution, not a continue — it must not spend a grant")
	}
	if m.turnStoppedByGuard {
		t.Fatal("ban stopped the turn")
	}
	told := false
	for _, msg := range m.history {
		if msg.Advisory && strings.Contains(msg.Content, "[TOOL DISABLED]") {
			told = true
		}
	}
	if !told {
		t.Fatal("the model was never told its tool was taken away")
	}
}

// The modal-spam cap: the same detector may be waved through twice per turn;
// its third crossing falls back to the automatic stop.
func TestLoopEscalationRearmsOnceThenCaps(t *testing.T) {
	m, call := repeatLoopTestModel(t)
	dispatchRepeatRound(m, call, 5)
	if m.state != stateLoopGuard {
		t.Fatal("first crossing should ask")
	}

	// Continue #1: the detector resets and the turn resumes.
	next, _ := m.resolveLoopEscalation(1)
	m = next.(*Model)
	cancelStream(m)
	if m.loopContinues[loopStopRepeat] != 1 {
		t.Fatalf("continue grant = %d, want 1", m.loopContinues[loopStopRepeat])
	}

	// The model keeps looping: four seeded rounds re-arm the streak, the fifth
	// re-crosses — and the user is asked again.
	seedRepeatRounds(m, call, 6, 7, 8, 9)
	dispatchRepeatRound(m, call, 10)
	if m.state != stateLoopGuard {
		t.Fatal("re-crossing after one continue should ask again")
	}

	next, _ = m.resolveLoopEscalation(1)
	m = next.(*Model)
	cancelStream(m)
	if m.loopContinues[loopStopRepeat] != 2 {
		t.Fatalf("continue grant = %d, want 2", m.loopContinues[loopStopRepeat])
	}

	// Third crossing: no third modal — the automatic stop takes over.
	seedRepeatRounds(m, call, 11, 12, 13, 14)
	dispatchRepeatRound(m, call, 15)
	defer cancelStream(m)
	if m.state == stateLoopGuard || m.loopEscalation != nil {
		t.Fatal("a third crossing must not ask a third time")
	}
	if !m.turnStoppedByGuard {
		t.Fatal("the capped escalation must fall back to the automatic stop")
	}
	if m.stream == nil || !m.stream.toolsSuppressed {
		t.Fatal("the automatic stop must stream the blocker report tool-less")
	}
}

// Unattended runs keep the old behavior exactly: auto mode (and, by
// construction, the headless and sub-agent loops, which never drive this
// dispatch path) stops on its own instead of parking on a modal nobody would
// answer.
func TestLoopEscalationSkipsModalWhenUnattended(t *testing.T) {
	m, call := repeatLoopTestModel(t)
	m.mode = AutoMode
	dispatchRepeatRound(m, call, 5)
	defer cancelStream(m)

	if m.state == stateLoopGuard || m.loopEscalation != nil {
		t.Fatal("auto mode must not park on a modal — the turn would hang unanswered")
	}
	if !m.turnStoppedByGuard {
		t.Fatal("auto mode must keep the automatic stop")
	}
	if m.stream == nil || !m.stream.toolsSuppressed {
		t.Fatal("the automatic stop still streams the blocker report tool-less")
	}
}

func TestChooseLoopGuardOption(t *testing.T) {
	withBan := []string{"Stop turn", "Continue anyway", "Ban todo_write & continue"}
	if got, ok := chooseLoopGuardOption("3", 0, withBan); !ok || got != 2 {
		t.Fatalf("digit picked %d ok=%v", got, ok)
	}
	if got, ok := chooseLoopGuardOption("enter", 1, withBan); !ok || got != 1 {
		t.Fatalf("enter picked %d ok=%v", got, ok)
	}
	// A digit naming a choice this modal does not have selects nothing: the
	// two-choice form (oscillation/stagnation) has no ban to pick.
	noBan := []string{"Stop turn", "Continue anyway"}
	if _, ok := chooseLoopGuardOption("3", 0, noBan); ok {
		t.Fatal("out-of-range digit selected a choice")
	}
	if _, ok := chooseLoopGuardOption("enter", 5, noBan); ok {
		t.Fatal("enter selected past the end of the list")
	}
	for _, key := range []string{"up", "down", "j", "k", "x", "0"} {
		if _, ok := chooseLoopGuardOption(key, 0, noBan); ok {
			t.Fatalf("key %q selected a choice", key)
		}
	}
}

func TestLoopGuardModalRendersReasonAndChoices(t *testing.T) {
	m := &Model{
		state: stateLoopGuard, width: 100, height: 40,
		md: newMarkdownRenderer(), notesMd: newMarkdownRenderer(),
		loopEscalation: &loopEscalation{kind: loopStopRepeat, tool: "todo_write", streak: 5},
	}
	out := m.loopGuardModal()
	for _, want := range []string{"Agent appears stuck", "repeated identical todo_write calls 5 times", "1. Stop turn", "2. Continue anyway", "3. Ban todo_write & continue", "esc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("modal missing %q:\n%s", want, out)
		}
	}

	m.loopEscalation = &loopEscalation{kind: loopStopStagnation, rounds: 4}
	out = m.loopGuardModal()
	if !strings.Contains(out, "no progress for 4 rounds") {
		t.Fatalf("modal missing the stagnation reason:\n%s", out)
	}
	if strings.Contains(out, "Ban") {
		t.Fatalf("stagnation must not offer a ban choice:\n%s", out)
	}
}

// A model repeating itself keeps the connection healthy, so no idle timeout
// ever fires. The output itself has to be the signal.
func TestStreamOutputRunaway(t *testing.T) {
	// Varied prose: every line differs, so no fragment recurs.
	var prose strings.Builder
	for i := range 200 {
		fmt.Fprintf(&prose, "step %d covers tui/mode.go and the policy table for case %d.\n", i, i*7)
	}
	if streamOutputRunaway(prose.String(), false) {
		t.Error("ordinary long prose flagged as a loop")
	}
	if streamOutputRunaway("", false) || streamOutputRunaway("short answer", false) {
		t.Error("short content flagged as a loop")
	}
	loop := strings.Repeat(`{"name":"switch_mode","arguments":{"mode":"write"}}`, 10)
	if !streamOutputRunaway(loop, false) {
		t.Error("repeated switch_mode transport not flagged")
	}
	repeated := strings.Repeat("I will now check the file to understand the structure. ", 40)
	if !streamOutputRunaway(repeated, false) {
		t.Error("a phrase repeated 40 times not flagged")
	}
	// A constrained reply is one call or one short envelope; its ceiling is
	// low enough that varied output past it still counts as a runaway.
	if !streamOutputRunaway(prose.String(), true) {
		t.Error("constrained reply past its ceiling not flagged")
	}
	// A long answer that repeats the same boilerplate line a few times across
	// the whole reply is not a loop — only a repeat at the tail is.
	var listing strings.Builder
	for i := range 400 {
		if i%80 == 0 {
			listing.WriteString("\tif err != nil {\n\t\treturn fmt.Errorf(\"read config: %w\", err)\n\t}\n")
			continue
		}
		fmt.Fprintf(&listing, "line %d of the listing, unique content for symbol_%d\n", i, i)
	}
	if streamOutputRunaway(listing.String(), false) {
		t.Error("repeated boilerplate spread across a long answer flagged as a loop")
	}
}
