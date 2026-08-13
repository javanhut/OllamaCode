package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func TestChatChunkQueuesMissingRenderFrame(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 2
	m.streaming = true
	m.lastRenderTime = time.Now()

	_, cmd := m.Update(chatChunkMsg{gen: 2, content: "new text"})
	if cmd == nil {
		t.Fatal("chunk inside cadence window did not schedule a render")
	}
	if !m.renderQueued {
		t.Fatal("scheduled render was not marked as queued")
	}
	if got := m.streamBuf.String(); got != "new text" {
		t.Fatalf("stream buffer = %q, want new text", got)
	}

	m.Update(streamRenderMsg{gen: 2})
	if m.renderQueued {
		t.Fatal("render message did not clear the queued marker")
	}
	if !strings.Contains(stripANSI(m.transcript.String()), "new text") {
		t.Fatal("scheduled frame did not paint buffered response text")
	}
}

func TestStructuredStreamIsHiddenUntilCompletion(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 2
	m.streaming = true
	m.stream = &streamState{gen: 2, cancel: func() {}}

	_, cmd := m.Update(chatChunkMsg{gen: 2, content: `{"response":"Hello there"}`})

	if cmd == nil || !m.stream.hideContent {
		t.Fatal("structured stream was not classified as hidden")
	}
	if rendered := stripANSI(m.transcript.String()); strings.Contains(rendered, `{"response"`) {
		t.Fatalf("raw response envelope leaked into transcript: %s", rendered)
	}
	m.stream.cancel()
}

func TestUnconstrainedResponseEnvelopeRendersCleanProse(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 1
	m.streaming = true
	m.stream = &streamState{gen: 1, hideContent: true, visibility: true, cancel: func() {}}
	m.streamBuf.WriteString(`{"response":"Hello there"}`)

	m.Update(chatDoneMsg{gen: 1})

	if len(m.history) == 0 || m.history[len(m.history)-1].Content != "Hello there" {
		t.Fatalf("response envelope was not cleaned: %#v", m.history)
	}
}

func TestLegitimateJSONObjectSurvivesCompletion(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 1
	m.streaming = true
	m.stream = &streamState{gen: 1, hideContent: true, visibility: true, cancel: func() {}}
	want := `{"response":"keep object","status":"ok"}`
	m.streamBuf.WriteString(want)

	m.Update(chatDoneMsg{gen: 1})

	if got := m.history[len(m.history)-1].Content; got != want {
		t.Fatalf("legitimate JSON changed: got %q want %q", got, want)
	}
}

func TestWaitForStreamKeepsThinkingAndContentFromSameFrame(t *testing.T) {
	responses := make(chan api.ChatResponse, 1)
	responses <- api.ChatResponse{Message: api.Message{
		Thinking: "reasoning",
		Content:  "answer",
	}}
	m := &Model{stream: &streamState{gen: 3, resp: responses}}

	msg, ok := m.waitForStream()().(chatChunkMsg)
	if !ok {
		t.Fatalf("stream frame returned %T, want chatChunkMsg", msg)
	}
	if msg.thinking != "reasoning" || msg.content != "answer" {
		t.Fatalf("stream frame lost a field: %#v", msg)
	}
}

func TestChatChunkCancelsRunawayOutput(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 3
	m.streaming = true
	cancelled := false
	m.stream = &streamState{gen: 3, constrained: true, cancel: func() { cancelled = true }}
	repeated := strings.Repeat(`{"mode":"write","reason":"building now"}...`, 12)

	_, cmd := m.Update(chatChunkMsg{gen: 3, content: repeated})

	if !cancelled || cmd == nil {
		t.Fatalf("runaway output was not cancelled: cancelled=%v cmd=%v", cancelled, cmd != nil)
	}
}

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

func TestIdleTimeoutSchedulesOneDegradedRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := statusTestModel()
	m.turnGen = 7
	m.contextLimit = maxContextBudget
	m.streaming = true
	m.stream = &streamState{gen: 7, cancel: func() {}}

	_, cmd := m.Update(chatErrMsg{gen: 7, err: errors.New("stream idle timeout after 90s — no response from local model")})

	if cmd == nil || m.streamRetries != 1 || !m.degradedStreamRetry {
		t.Fatalf("idle timeout did not schedule degraded retry: retries=%d degraded=%v", m.streamRetries, m.degradedStreamRetry)
	}
	if !strings.Contains(m.toast, "(1/1)") {
		t.Fatalf("idle timeout should allow one retry, got toast %q", m.toast)
	}
	if got := m.requestContextLimit(); got != defaultContextLimit {
		t.Fatalf("degraded retry context = %d, want %d", got, defaultContextLimit)
	}
}

func TestRunawayErrorDisablesConstraintAndRetriesOnce(t *testing.T) {
	m := statusTestModel()
	m.turnGen = 7
	m.modelName = "runaway-small"
	m.profile = ModelProfile{ParamsB: 7, SupportsTools: true}
	m.streaming = true
	m.stream = &streamState{gen: 7, constrained: true, cancel: func() {}}

	_, cmd := m.Update(chatErrMsg{gen: 7, err: errRunawayModelStream})

	if cmd == nil || m.streamRetries != 1 || !m.degradedStreamRetry {
		t.Fatalf("runaway did not schedule one degraded retry: retries=%d degraded=%v", m.streamRetries, m.degradedStreamRetry)
	}
	if got := m.history[len(m.history)-1].Content; !strings.Contains(got, "RETRY CORRECTION") {
		t.Fatalf("missing retry correction: %q", got)
	}
	if raw, ok := m.toolCallFormat(true, testConstraintDefs(t)); ok {
		t.Fatalf("runaway constraint was not disabled, got %s", raw)
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

func TestChatDoneContinuesADeferredToolAction(t *testing.T) {
	m := statusTestModel()
	m.modelName = "test-model"
	m.profile = ModelProfile{NumCtx: 8192, ParamsB: 7, SupportsTools: true}
	m.contextLimit = 8192
	m.history = append(m.history, api.Message{Role: "user", Content: "build a snake game"})
	m.turnGen = 1
	m.streaming = true
	m.stream = &streamState{gen: 1, tools: true, cancel: func() {}}

	_, cmd := m.Update(chatDoneMsg{gen: 1, content: "Let me check the current workspace first."})

	if cmd == nil || m.actionDeferrals != 1 || !m.streaming || m.stream == nil {
		t.Fatalf("deferred action was not continued: deferrals=%d streaming=%v", m.actionDeferrals, m.streaming)
	}
	if got := m.history[len(m.history)-1].Content; !strings.Contains(got, "ACTION REQUIRED") {
		t.Fatalf("missing corrective tool instruction: %q", got)
	}
	m.stream.cancel()
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
	bandW := 80 - lipgloss.Width(m.inputPrefix())
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

// The prefix used to grow from "message" to "queued while streaming", which
// re-wrapped whatever was already typed the moment a stream started.
func TestInputPrefixWidthStableWhileStreaming(t *testing.T) {
	m := statusTestModel()
	idle := lipgloss.Width(m.inputPrefix())
	m.streaming = true
	if busy := lipgloss.Width(m.inputPrefix()); busy != idle {
		t.Fatalf("prefix width changed while streaming: %d -> %d", idle, busy)
	}
}

// Every row of a grown input must start past the label gutter, not at column 0.
func TestInputBandRowsAlignUnderPrefix(t *testing.T) {
	m := statusTestModel()
	m.input.SetValue(strings.Repeat("x", 300))
	m.layout()
	band := m.inputPrefixColumn(lipgloss.Height(m.input.View()))
	rows := strings.Split(band, "\n")
	if len(rows) < 2 {
		t.Fatalf("expected the input to wrap to multiple rows, got %d", len(rows))
	}
	want := lipgloss.Width(m.inputPrefix())
	for i, r := range rows {
		if got := lipgloss.Width(r); got != want {
			t.Fatalf("gutter row %d width = %d, want %d", i, got, want)
		}
	}
}

// Arrowing around inside a message must not swap it for a history entry.
func TestArrowKeysDoNotClobberTypedInput(t *testing.T) {
	m := statusTestModel()
	m.userHistory = []string{"older message"}
	m.historyIndex = len(m.userHistory)
	m.input.SetValue("line one\nline two")
	m.input.CursorEnd()

	for name, code := range map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown} {
		before := m.input.Value()
		mm, _ := m.Update(tea.KeyPressMsg{Code: code})
		got := mm.(*Model).input.Value()
		if got != before {
			t.Fatalf("%q rewrote the buffer: %q -> %q", name, before, got)
		}
	}
}

// ...but on the first row with an untouched buffer, up still recalls history.
func TestUpRecallsHistoryFromFirstRow(t *testing.T) {
	m := statusTestModel()
	m.userHistory = []string{"older message"}
	m.historyIndex = len(m.userHistory)

	mm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := mm.(*Model).input.Value(); got != "older message" {
		t.Fatalf("up did not recall history, got %q", got)
	}
}
