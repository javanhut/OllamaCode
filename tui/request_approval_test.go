package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

// approvalTestModel is a plan-mode model with a recorded plan, ready for a
// request_approval dispatch.
func approvalTestModel(summaryArgs string) *Model {
	m := interruptTestModel()
	m.mode = PlanMode
	m.notes.set("1. edit tui/mode.go\n2. add a test")
	call := tc("request_approval", summaryArgs)
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}
	return m
}

func historyContains(m *Model, substr string) bool {
	for _, msg := range m.history {
		if strings.Contains(msg.Content, substr) {
			return true
		}
	}
	return false
}

// request_approval pauses the batch at a permission prompt showing the plan,
// exactly like the destructive-tool permission path.
func TestRequestApprovalPromptsWithRecordedPlan(t *testing.T) {
	t.Run("summary arg is the preview", func(t *testing.T) {
		m := approvalTestModel(`{"summary":"Patch mode transitions"}`)
		if cmd := m.processPendingTools(); cmd != nil {
			t.Fatal("request_approval should pause for the user, not execute")
		}
		if m.state != statePermission {
			t.Fatalf("state = %v, want statePermission", m.state)
		}
		if m.pending.preview != "Patch mode transitions" {
			t.Fatalf("preview = %q, want the summary arg", m.pending.preview)
		}
	})

	t.Run("empty summary falls back to the notes", func(t *testing.T) {
		m := approvalTestModel(`{}`)
		if cmd := m.processPendingTools(); cmd != nil {
			t.Fatal("request_approval should pause for the user, not execute")
		}
		if m.pending.preview != m.notes.get() {
			t.Fatalf("preview = %q, want the session notes", m.pending.preview)
		}
	})
}

// Approval is one shot: it counts as the plan review AND switches the session
// to write mode; the stub handler never runs.
func TestRequestApprovalApproveSwitchesToWrite(t *testing.T) {
	m := approvalTestModel(`{"summary":"Patch mode transitions"}`)
	m.processPendingTools()

	_, _ = m.updatePermission(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.stream != nil {
		defer m.stream.cancel()
	}

	if m.mode != WriteMode {
		t.Fatalf("mode = %s, want write after approval", m.mode)
	}
	if m.planReviewed != strings.TrimSpace(m.notes.get()) {
		t.Fatalf("planReviewed = %q, want the current notes", m.planReviewed)
	}
	if !m.planNeedsVerify || !m.planPaths["tui/mode.go"] {
		t.Fatalf("plan verification was not armed: verify=%v paths=%v", m.planNeedsVerify, m.planPaths)
	}
	if m.pending != nil {
		t.Fatal("the batch should have finalized after approval")
	}
	if !historyContains(m, "plan approved by the user") {
		t.Fatalf("no tool result recorded for the approval: %#v", m.history)
	}
	if !historyContains(m, "Plan Summary from Session Notes") {
		t.Fatal("the plan was not handed off to write mode")
	}
}

// "Allow all" has no meaning for a one-shot approval; it approves like y.
func TestRequestApprovalAllowAllActsAsApprove(t *testing.T) {
	m := approvalTestModel(`{}`)
	m.processPendingTools()

	_, _ = m.updatePermission(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if m.stream != nil {
		defer m.stream.cancel()
	}
	if m.mode != WriteMode {
		t.Fatalf("mode = %s, want write after 'a' on request_approval", m.mode)
	}
	if m.pending != nil && m.pending.allowAll {
		t.Fatal("allowAll must not suppress the rest of the turn's prompts")
	}
}

// Denial reuses the existing turn-ending path: the model gets the rejection as
// feedback and the user can type what to change.
func TestRequestApprovalDenialEndsTurnWithFeedback(t *testing.T) {
	m := approvalTestModel(`{}`)
	m.processPendingTools()

	_, _ = m.updatePermission(tea.KeyPressMsg{Code: 'n', Text: "n"})

	if m.mode != PlanMode {
		t.Fatalf("mode = %s, denial must not leave plan mode", m.mode)
	}
	if m.planReviewed != "" {
		t.Fatalf("planReviewed = %q after denial", m.planReviewed)
	}
	if m.pending != nil {
		t.Fatal("the batch should have finalized after denial")
	}
	if !historyContains(m, "denied by user") {
		t.Fatal("the denial was not recorded as the tool result")
	}
	if !historyContains(m, "denied request_approval") {
		t.Fatal("no denial feedback prompt for the user")
	}
	if m.denialFeedbackTool != "request_approval" {
		t.Fatalf("denialFeedbackTool = %q", m.denialFeedbackTool)
	}
}

// request_approval without a recorded plan is refused with instructions, not
// a prompt — the model is meant to write the notes and retry the exact call.
func TestRequestApprovalRequiresRecordedPlan(t *testing.T) {
	m := interruptTestModel()
	m.mode = PlanMode
	call := tc("request_approval", `{"summary":"nothing recorded yet"}`)
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}

	cmd := m.processPendingTools()
	if m.stream != nil {
		defer m.stream.cancel()
	}
	if m.state == statePermission {
		t.Fatal("request_approval without a plan must not prompt the user")
	}
	if cmd == nil {
		t.Fatal("the finalized batch should continue the turn")
	}
	if !historyContains(m, "no plan recorded") || !historyContains(m, "update_session_notes") {
		t.Fatalf("refusal did not direct the model to record the plan: %#v", m.history)
	}
	if m.failedCalls[tools.CallFingerprint(call)] != 0 {
		t.Fatal("a missing-plan refusal must not count against the model — it is told to retry")
	}
}

// Outside plan mode the tool is refused outright.
func TestRequestApprovalRefusedOutsidePlanMode(t *testing.T) {
	m := interruptTestModel() // explore mode
	call := tc("request_approval", `{}`)
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}

	m.processPendingTools()
	if m.stream != nil {
		defer m.stream.cancel()
	}
	if m.state == statePermission {
		t.Fatal("request_approval outside plan mode must not prompt")
	}
	if !historyContains(m, "only available in plan mode") {
		t.Fatalf("expected a plan-mode-only refusal: %#v", m.history)
	}
}

// A dispatch-level refusal never reaches invokeTool, so without its own trace
// event it is invisible in the debug log — the plan-gate deadlock this test
// covers was diagnosed from exactly that gap.
func TestRefusedToolCallEmitsTraceEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	recorder, err := tracepkg.OpenFresh(path)
	if err != nil {
		t.Fatal(err)
	}

	m := interruptTestModel()
	m.mode = PlanMode
	m.trace = recorder
	// The plan gate refuses: no plan recorded.
	call := tc("switch_mode", `{"mode":"write","reason":"ready"}`)
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}
	m.processPendingTools()
	if m.stream != nil {
		defer m.stream.cancel()
	}
	_ = recorder.Close()

	var refused []tracepkg.Event
	if err := tracepkg.Replay(path, func(event tracepkg.Event) error {
		if event.Kind == "tool" && event.Metadata["refused"] == true {
			refused = append(refused, event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(refused) != 1 {
		t.Fatalf("expected exactly one refused tool event, got %d", len(refused))
	}
	event := refused[0]
	if event.Tool != "switch_mode" {
		t.Fatalf("refused event tool = %q", event.Tool)
	}
	if !strings.Contains(event.Result, "no plan recorded") {
		t.Fatalf("refusal reason not in the event result: %q", event.Result)
	}
}

// Plan-mode turns are meant to end for user review; the open-todo [CONTINUE]
// nudge would loop-lock the model into re-presenting the same plan.
func TestContinueNudgeDoesNotFireInPlanMode(t *testing.T) {
	build := func(mode Mode) *Model {
		m := &Model{
			mode: mode, turnGen: 1, maxSteps: defaultMaxSteps,
			tools: tools.NewRegistry(), notes: &sessionNotes{}, todos: &todoList{},
			failedCalls: map[string]int{},
			transcript:  &strings.Builder{}, streamBuf: &strings.Builder{},
			md: newMarkdownRenderer(), notesMd: newMarkdownRenderer(),
		}
		m.todos.set([]todoItem{{Content: "still open", Status: todoInProgress}})
		m.viewport.SetWidth(80)
		return m
	}

	plan := build(PlanMode)
	next, _ := plan.Update(chatDoneMsg{gen: 1, content: "Here is the plan for your review."})
	after := next.(*Model)
	if after.stream != nil {
		defer after.stream.cancel()
	}
	if after.autoContinues != 0 {
		t.Fatalf("plan-mode turn was auto-continued %d time(s)", after.autoContinues)
	}
	if historyContains(after, "[CONTINUE]") {
		t.Fatal("plan-mode turn was nudged back into its open todos")
	}

	// Control: the same turn in write mode still earns the nudge.
	write := build(WriteMode)
	next, _ = write.Update(chatDoneMsg{gen: 1, content: "I made the change."})
	after = next.(*Model)
	if after.stream != nil {
		defer after.stream.cancel()
	}
	if after.autoContinues != 1 {
		t.Fatalf("write-mode auto-continues = %d, want 1", after.autoContinues)
	}
	if !historyContains(after, "[CONTINUE]") {
		t.Fatal("write-mode turn with open todos lost its [CONTINUE] nudge")
	}
}
