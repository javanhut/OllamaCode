package tui

import (
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// statusTestModel is interruptTestModel plus the bits layout()/Update() touch
// (markdown notes renderer, focused textarea, real viewport via layout).
func statusTestModel() *Model {
	m := interruptTestModel()
	m.notesMd = newMarkdownRenderer()
	m.input = textarea.New()
	m.width, m.height = 80, 24
	m.layout()
	return m
}

func TestStatusTextPhases(t *testing.T) {
	pending := &pendingBatch{
		calls:   []tools.ToolCall{tc("read_file", `{"path":"a.txt"}`)},
		results: make([]api.Message, 1),
		started: make([]bool, 1),
	}
	cases := []struct {
		name  string
		setup func(m *Model)
		want  string
		busy  bool
	}{
		{"idle", func(m *Model) {}, "READY", false},
		{"thinking", func(m *Model) { m.streaming = true }, "THINKING", true},
		{"streaming with content", func(m *Model) {
			m.streaming = true
			m.streamBuf.WriteString("hi")
		}, "STREAMING", true},
		{"retrieving", func(m *Model) { m.retrieving = true }, "SEARCHING CODE", true},
		{"compacting", func(m *Model) { m.compacting = true }, "COMPACTING", true},
		{"verifying", func(m *Model) { m.verifying = true }, "VERIFYING", true},
		{"retrieving beats streaming", func(m *Model) {
			m.retrieving = true
			m.streaming = true
		}, "SEARCHING CODE", true},
		{"tools beat retrieving", func(m *Model) {
			m.pending = pending
			m.retrieving = true
		}, "TOOLS 0/1 · read_file", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := interruptTestModel()
			c.setup(m)
			text, busy := m.statusText()
			if text != c.want {
				t.Fatalf("statusText() = %q, want %q", text, c.want)
			}
			if busy != c.busy {
				t.Fatalf("statusText() busy = %v, want %v", busy, c.busy)
			}
		})
	}
}

func TestChatErrSchedulesBackoffRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep logActivity's config write out of the real HOME
	m := statusTestModel()
	m.turnGen = 7
	m.streaming = true
	m.stream = &streamState{gen: 7, cancel: func() {}}

	_, cmd := m.Update(chatErrMsg{gen: 7, err: errors.New("connection reset")})

	if m.streamRetries != 1 {
		t.Fatalf("streamRetries = %d, want 1", m.streamRetries)
	}
	if cmd == nil {
		t.Fatal("expected a backoff tick command for the retry")
	}
	if m.turnGen != 7 {
		t.Fatal("retry fired startStream immediately — expected a backoff tick first")
	}
	if !strings.HasPrefix(m.toast, "stream error") || !strings.Contains(m.toast, "retrying (1/2) in 2s") {
		t.Fatalf("unexpected retry toast %q", m.toast)
	}
}

func TestRetryStreamMsgStartsStream(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 3

	_, cmd := m.Update(retryStreamMsg{gen: 3})

	if cmd == nil {
		t.Fatal("expected the retry to start a new stream")
	}
	if !m.streaming || m.stream == nil {
		t.Fatal("retry did not start a stream")
	}
	if m.turnGen != 4 {
		t.Fatalf("startStream should bump the turn generation, got %d", m.turnGen)
	}
	m.stream.cancel()
}

func TestRetryStreamMsgStaleGenDropped(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 4 // turn advanced past the scheduled retry (e.g. user interrupted)

	m.Update(retryStreamMsg{gen: 3})

	if m.streaming || m.stream != nil {
		t.Fatal("stale retry started a stream")
	}
}

func TestChatDoneClearsRetryToast(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 1
	m.streaming = true
	m.stream = &streamState{gen: 1, cancel: func() {}}
	m.toast = "stream error — retrying (1/2) in 2s…"

	m.Update(chatDoneMsg{gen: 1, content: "all better"})

	if m.toast != "" {
		t.Fatalf("retry toast should clear on success, got %q", m.toast)
	}
	if m.streaming {
		t.Fatal("stream should be marked done")
	}
}

func TestChatDoneKeepsOtherToasts(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 1
	m.streaming = true
	m.stream = &streamState{gen: 1, cancel: func() {}}
	m.toast = "context compacted"

	m.Update(chatDoneMsg{gen: 1, content: "done"})

	if m.toast != "context compacted" {
		t.Fatalf("unrelated toast should survive, got %q", m.toast)
	}
}

func TestNarrowStatusLine(t *testing.T) {
	m := interruptTestModel()
	m.width = 80
	if got := m.narrowStatusLine(); got != "" {
		t.Fatalf("wide terminal should leave status to the sidebar, got %q", got)
	}

	m.width = 50
	if got := m.narrowStatusLine(); !strings.Contains(got, "READY") {
		t.Fatalf("idle narrow status should show READY, got %q", got)
	}
	m.streaming = true
	if got := m.narrowStatusLine(); !strings.Contains(got, "THINKING") {
		t.Fatalf("busy narrow status should show the phase, got %q", got)
	}
	m.toast = "compacting & compressing..."
	got := m.narrowStatusLine()
	if !strings.Contains(got, m.toast) {
		t.Fatalf("narrow status should surface the toast, got %q", got)
	}
	if lipgloss.Height(got) != 1 {
		t.Fatalf("narrow status must stay one line, got %d", lipgloss.Height(got))
	}
}

func TestTranscriptPhaseSpinners(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *Model)
		want  string
	}{
		{"retrieving", func(m *Model) { m.retrieving = true }, "Searching code..."},
		{"compacting", func(m *Model) { m.compacting = true }, "Compacting context..."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := interruptTestModel()
			m.history = []api.Message{{Role: "user", Content: "hello"}}
			c.setup(m)
			m.refreshTranscript()
			if !strings.Contains(m.transcript.String(), c.want) {
				t.Fatalf("transcript missing %q:\n%s", c.want, m.transcript.String())
			}
		})
	}
}

func TestTranscriptVerifyingLine(t *testing.T) {
	m := interruptTestModel()
	m.history = []api.Message{
		{Role: "user", Content: "fix it"},
		{Role: "assistant", Content: "done, I fixed it"},
	}
	m.verifying = true
	m.refreshTranscript()
	if !strings.Contains(m.transcript.String(), "verifying…") {
		t.Fatalf("transcript missing the verifying line:\n%s", m.transcript.String())
	}
}

func TestLayoutSizesTextareaToPrefix(t *testing.T) {
	m := statusTestModel() // width 80, laid out
	bandW := 80 - lipgloss.Width(m.inputPrefix()) - 2
	m.input.SetValue(strings.Repeat("x", 300))
	for i, line := range strings.Split(m.input.View(), "\n") {
		if w := lipgloss.Width(line); w > bandW {
			t.Fatalf("textarea line %d width = %d, exceeds band width %d", i, w, bandW)
		}
	}
	for i, line := range strings.Split(m.inputView(), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("inputView line %d width = %d, exceeds terminal width 80", i, w)
		}
	}
}
