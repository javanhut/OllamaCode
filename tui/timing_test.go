package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javanhut/ollama_code/api"
)

func TestShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{840 * time.Millisecond, "840ms"},
		{6100 * time.Millisecond, "6.1s"},
		{124 * time.Second, "2m04s"},
		{-time.Second, "0ms"},
	}
	for _, c := range cases {
		if got := shortDuration(c.d); got != c.want {
			t.Errorf("shortDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTimingFooterSplitsThinkAndTools(t *testing.T) {
	m := &Model{turnRecords: map[int]turnRecord{
		0: {total: 8 * time.Second, tools: 2 * time.Second, calls: 3},
		2: {total: 4500 * time.Millisecond},
	}}
	got := stripANSI(m.timingFooter(0))
	if want := "⏱ 8.0s · think 6.0s · tools 2.0s (3 calls)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// No tools ran: don't pretend the split is interesting.
	if got, want := stripANSI(m.timingFooter(2)), "⏱ 4.5s"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if m.timingFooter(1) != "" {
		t.Error("untimed turn should render nothing")
	}
}

// A turn's timing is keyed by its position in the log, and compaction no longer
// moves anything: it advances a boundary. The rebasing this test used to cover
// existed because compaction sliced the front off history, and every index-keyed
// reader had to be told. Nothing to tell now — that is the point.
func TestTurnTimesSurviveCompaction(t *testing.T) {
	m := overflowTestModel(t)
	m.turnRecords = map[int]turnRecord{
		0: {total: time.Second},
		4: {total: 2 * time.Second},
		6: {total: 3 * time.Second},
	}
	m.turnAnchor = 6
	logged := len(m.history)

	m.Update(compactDoneMsg{summary: "the earlier conversation", index: 4})

	if len(m.history) != logged {
		t.Fatalf("compaction mutated the log: %d -> %d", logged, len(m.history))
	}
	if m.archivedThrough != 4 {
		t.Fatalf("archive boundary = %d, want 4", m.archivedThrough)
	}
	if len(m.deriveModelMessages()) != logged-4 {
		t.Fatalf("model view = %d messages, want %d", len(m.deriveModelMessages()), logged-4)
	}
	for _, key := range []int{0, 4, 6} {
		if _, ok := m.turnRecords[key]; !ok {
			t.Errorf("timing at index %d was disturbed: %v", key, m.turnRecords)
		}
	}
	if m.turnAnchor != 6 {
		t.Errorf("turn anchor moved to %d", m.turnAnchor)
	}
}

// The clock anchors on the user message that started the turn and lands in the
// transcript under that turn's answer.
func TestTurnClockLandsInTranscript(t *testing.T) {
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := mm.(*Model)
	m.history = []api.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "second"},
	}
	m.startTurnClock()
	if m.turnAnchor != 2 {
		t.Fatalf("anchor = %d, want 2 (the newest user message)", m.turnAnchor)
	}
	m.markToolsStart(2)
	m.markToolsDone()
	m.history = append(m.history, api.Message{Role: "assistant", Content: "answer two"})
	m.finishTurnClock()

	got, ok := m.turnRecords[2]
	if !ok {
		t.Fatal("no timing recorded for the turn")
	}
	if got.calls != 2 || got.total <= 0 {
		t.Errorf("timing = %+v, want 2 calls and a positive total", got)
	}
	m.refreshTranscript()
	if !strings.Contains(stripANSI(m.transcript.String()), "⏱") {
		t.Error("transcript shows no timing footer")
	}
}

// /show_thinking replays reasoning captured earlier in the session; off, it
// leaves no trace in the transcript.
func TestShowThinkingToggle(t *testing.T) {
	// The toggle persists via saveConfig — keep the real config out of it.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	mm, _ := New().Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m := mm.(*Model)
	m.history = []api.Message{
		{Role: "user", Content: "why?"},
		{Role: "assistant", Content: "because."},
	}
	m.turnRecords = map[int]turnRecord{0: {total: time.Second, thinking: "weighing the options"}}

	m.refreshTranscript()
	if strings.Contains(stripANSI(m.transcript.String()), "weighing the options") {
		t.Error("reasoning shown while the toggle is off")
	}

	m = typeKeys(t, m, "/show_thinking")
	m = press(t, m, tea.KeyEnter, 0)
	if !m.cfg.Thinking {
		t.Fatal("/show_thinking did not turn the toggle on")
	}
	if got := stripANSI(m.transcript.String()); !strings.Contains(got, "weighing the options") ||
		!strings.Contains(got, "thinking") {
		t.Errorf("reasoning not replayed:\n%s", got)
	}

	m = typeKeys(t, m, "/show_thinking")
	m = press(t, m, tea.KeyEnter, 0)
	if m.cfg.Thinking {
		t.Error("second /show_thinking did not turn it back off")
	}
	if strings.Contains(stripANSI(m.transcript.String()), "weighing the options") {
		t.Error("reasoning still shown after toggling off")
	}
}

func TestShowThinkingWhileStreaming(t *testing.T) {
	m := &Model{
		cfg:        config{Thinking: true},
		streaming:  true,
		history:    []api.Message{{Role: "user", Content: "why?"}},
		streamBuf:  &strings.Builder{},
		transcript: &strings.Builder{},
		md:         newMarkdownRenderer(),
	}
	m.viewport.SetWidth(80)
	m.turnThinking.WriteString("checking the first option")
	m.streamBuf.WriteString("The answer has started.")

	m.refreshTranscript()
	got := stripANSI(m.transcript.String())
	if !strings.Contains(got, "thinking · live") || !strings.Contains(got, "checking the first option") {
		t.Fatalf("live reasoning was not rendered as an isolated block:\n%s", got)
	}
	if !strings.Contains(got, "└\nThe answer has started.") {
		t.Fatalf("reasoning block was not closed before answer text:\n%s", got)
	}
}

// Reasoning is captured per turn regardless of the toggle, and bounded.
func TestRecordThinkingIsBounded(t *testing.T) {
	m := &Model{}
	m.history = []api.Message{{Role: "user", Content: "go"}}
	m.startTurnClock()
	for range 100 {
		m.recordThinking(strings.Repeat("x", 1024))
	}
	m.finishTurnClock()
	got := len(m.turnRecords[0].thinking)
	if got == 0 || got > maxTurnThinking+1024 {
		t.Errorf("kept %d bytes of reasoning, want 0 < n <= %d", got, maxTurnThinking+1024)
	}
}
