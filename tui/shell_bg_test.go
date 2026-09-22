package tui

import (
	"strings"
	"testing"
)

// Mirror of TestSubagentCompletionNotifies for background shell jobs: the
// completion lands in history as a system message, a toast is set, the event
// waiter is re-armed, and an idle parent is woken with a new stream so it can
// react to the finished job.
func TestShellJobCompletionNotifies(t *testing.T) {
	m := subagentTestModel()
	m.modelName = "test-model" // non-empty so the idle wake path can fire
	m.shellJobEvents = make(chan shellJobDoneMsg, 1)

	_, cmd := m.Update(shellJobDoneMsg{id: 2, exitCode: 0, tail: "build ok"})

	if len(m.history) != 1 {
		t.Fatalf("expected the completion notification in history, got %#v", m.history)
	}
	msg := m.history[0]
	if msg.Role != "system" ||
		!strings.Contains(msg.Content, "[SHELL JOB 2 COMPLETE exit=0]") ||
		!strings.Contains(msg.Content, "build ok") {
		t.Fatalf("unexpected notification message: %#v", msg)
	}
	if !strings.Contains(m.toast, "shell job 2 finished") {
		t.Fatalf("unexpected toast %q", m.toast)
	}
	if cmd == nil {
		t.Fatal("expected commands (re-armed waiter + wake stream)")
	}
	if !m.streaming || m.stream == nil {
		t.Fatal("idle parent should have been woken with a new stream")
	}
	m.stream.cancel()
}

// A mid-turn completion must land in history without disturbing the stream —
// the model sees it at its next step.
func TestShellJobCompletionDoesNotWakeBusy(t *testing.T) {
	m := subagentTestModel()
	m.modelName = "test-model"
	m.streaming = true
	m.stream = &streamState{cancel: func() {}}

	m.Update(shellJobDoneMsg{id: 3, exitCode: 1, tail: "boom"})

	if len(m.history) != 1 || !strings.Contains(m.history[0].Content, "SHELL JOB 3 FAILED exit=1") {
		t.Fatalf("notification missing while busy: %#v", m.history)
	}
	// The pre-existing stream must be untouched (no second stream started).
	if m.stream == nil || m.stream.cancel == nil {
		t.Fatal("busy parent's stream was disturbed")
	}
}

// A killed job (shell_output kill=true → signal) renders as KILLED.
func TestShellJobKilledOutcome(t *testing.T) {
	msg := shellJobDoneMsg{id: 4, exitCode: -1, err: "signal: killed"}
	if got := msg.outcome(); got != "KILLED" {
		t.Fatalf("unexpected outcome %q", got)
	}
	if !strings.Contains(msg.notification(), "[SHELL JOB 4 KILLED]") {
		t.Fatalf("unexpected notification %q", msg.notification())
	}
	if !strings.Contains(msg.toastLine(), "shell job 4 killed") {
		t.Fatalf("unexpected toast %q", msg.toastLine())
	}
}
