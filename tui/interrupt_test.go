package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func interruptTestModel() *Model {
	return &Model{
		mode:        ExploreMode,
		state:       stateChat,
		tools:       tools.DefaultRegistry(),
		notes:       &sessionNotes{},
		transcript:  &strings.Builder{},
		streamBuf:   &strings.Builder{},
		md:          newMarkdownRenderer(),
		failedCalls: make(map[string]int),
		maxSteps:    25,
	}
}

func TestInterruptRunsQueuedMessageNext(t *testing.T) {
	cancelled := false
	m := interruptTestModel()
	m.streaming = true
	m.stream = &streamState{cancel: func() { cancelled = true }}
	m.queue = []string{"first queued", "second queued"}
	m.streamBuf.WriteString("partial reply")

	cmd := m.interruptTurn()

	if !cancelled {
		t.Fatal("interrupt did not cancel the in-flight stream")
	}
	if m.streamBuf.Len() != 0 {
		t.Fatal("interrupt did not reset the stream buffer")
	}
	if len(m.queue) != 1 || m.queue[0] != "second queued" {
		t.Fatalf("expected only the head to dequeue, queue=%v", m.queue)
	}
	if len(m.history) != 1 || m.history[0].Role != "user" || m.history[0].Content != "first queued" {
		t.Fatalf("queued head not appended to history: %#v", m.history)
	}
	if m.toast != "stopped — running queued message" {
		t.Fatalf("unexpected toast %q", m.toast)
	}
	if cmd == nil {
		t.Fatal("expected a stream command for the dequeued message")
	}
	if !m.streaming || m.stream == nil {
		t.Fatal("dequeued message did not start a new stream")
	}
	m.stream.cancel()
}

func TestInterruptWithoutQueueJustStops(t *testing.T) {
	m := interruptTestModel()
	m.streaming = true
	m.stream = &streamState{cancel: func() {}}
	m.state = statePermission
	m.pending = &pendingBatch{}

	cmd := m.interruptTurn()

	if cmd != nil {
		t.Fatal("expected no follow-up command with an empty queue")
	}
	if m.streaming || m.stream != nil || m.pending != nil {
		t.Fatal("interrupt left turn state set")
	}
	if m.state != stateChat {
		t.Fatalf("interrupt should leave the permission prompt, got state %v", m.state)
	}
	if m.toast != "stopped" {
		t.Fatalf("unexpected toast %q", m.toast)
	}
}

func TestSubmitWithNonEmptyQueueStaysFIFO(t *testing.T) {
	m := interruptTestModel()
	m.queue = []string{"waiting"}
	m.input = textarea.New()
	m.input.SetValue("new submission")

	cmd := m.submit()

	if len(m.queue) != 1 || m.queue[0] != "new submission" {
		t.Fatalf("new submission should line up behind the queue, queue=%v", m.queue)
	}
	if len(m.history) != 1 || m.history[0].Content != "waiting" {
		t.Fatalf("queue head should start first, history=%#v", m.history)
	}
	if cmd == nil {
		t.Fatal("expected a stream command for the dequeued head")
	}
	if m.stream != nil {
		m.stream.cancel()
	}
}

func TestDenyPermissionRecordsFailure(t *testing.T) {
	call := tc("write_file", `{"path":"a.txt","content":"x"}`)
	m := interruptTestModel()
	m.mode = WriteMode
	m.state = statePermission
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}

	_, cmd := m.updatePermission(tea.KeyPressMsg{Code: 'n', Text: "n"})

	fp := tools.CallFingerprint(call)
	if m.failedCalls[fp] != 1 {
		t.Fatalf("denial should count as a failed call, got %d", m.failedCalls[fp])
	}
	if m.state != stateChat {
		t.Fatalf("expected stateChat after denial, got %v", m.state)
	}
	// The batch finalizes into history once every call is done.
	if len(m.history) == 0 {
		t.Fatal("denied batch never finalized into history")
	}
	res := m.history[0]
	if res.Role != "tool" || !strings.Contains(res.Content, "Do NOT retry") {
		t.Fatalf("denial result missing anti-retry guidance: %#v", res)
	}
	if cmd == nil {
		t.Fatal("expected the turn to continue after denial")
	}
	if m.stream != nil {
		m.stream.cancel()
	}
}

func TestDeniedCallShortCircuitsOnThirdAttempt(t *testing.T) {
	call := tc("write_file", `{"path":"a.txt","content":"x"}`)
	m := interruptTestModel()
	m.mode = WriteMode
	m.state = statePermission

	deny := func() {
		m.pending = &pendingBatch{
			calls:   []tools.ToolCall{call},
			results: make([]api.Message, 1),
			started: make([]bool, 1),
		}
		m.updatePermission(tea.KeyPressMsg{Code: 'n', Text: "n"})
		if m.stream != nil {
			m.stream.cancel()
			m.stream = nil
		}
		m.streaming = false
		m.state = statePermission
		m.history = m.history[:0]
	}
	deny()
	deny()

	// Third identical call: no permission prompt — the failedCalls
	// short-circuit rejects it synchronously with a "do not repeat" message.
	m.state = stateChat
	m.pending = &pendingBatch{
		calls:   []tools.ToolCall{call},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}
	cmd := m.processPendingTools()
	if m.stream != nil {
		m.stream.cancel()
	}
	if m.state == statePermission {
		t.Fatal("third identical attempt should not re-prompt for permission")
	}
	if cmd == nil {
		t.Fatal("expected the rejected batch to advance to the next model stream")
	}
	found := false
	for _, msg := range m.history {
		if msg.Role == "tool" && strings.Contains(msg.Content, "Do not repeat it") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the identical-call short-circuit message, history=%#v", m.history)
	}
}
