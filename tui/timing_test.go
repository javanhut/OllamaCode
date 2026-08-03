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
	m := &Model{turnTimes: map[int]turnTiming{
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

// A turn's timing has to survive compaction, which drops messages off the front
// of history and shifts every index that timings are keyed by.
func TestRebaseTurnTimesAfterCompaction(t *testing.T) {
	m := &Model{
		turnAnchor: 6,
		turnTimes: map[int]turnTiming{
			0: {total: time.Second},
			4: {total: 2 * time.Second},
			6: {total: 3 * time.Second},
		},
	}
	m.rebaseTurnTimes(4)
	if _, ok := m.turnTimes[0]; !ok || m.turnTimes[0].total != 2*time.Second {
		t.Errorf("index 4 did not move to 0: %v", m.turnTimes)
	}
	if m.turnTimes[2].total != 3*time.Second {
		t.Errorf("index 6 did not move to 2: %v", m.turnTimes)
	}
	if len(m.turnTimes) != 2 {
		t.Errorf("dropped turns should be forgotten: %v", m.turnTimes)
	}
	if m.turnAnchor != 2 {
		t.Errorf("live anchor = %d, want 2", m.turnAnchor)
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

	got, ok := m.turnTimes[2]
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
