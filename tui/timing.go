package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Per-turn timing. Every user command is clocked end to end and split into the
// part spent running tools and the part spent waiting on the model, so a slow
// turn says which half was slow. Timings are keyed by the index of the user
// message that started the turn — refreshTranscript re-renders from history, so
// the number has to be recoverable, not just remembered for the latest answer.

// startTurnClock begins timing a fresh user turn. Called from resetTurnGuards,
// which every turn entry point already goes through.
func (m *Model) startTurnClock() {
	m.turnAnchor = -1
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "user" {
			m.turnAnchor = i
			break
		}
	}
	m.turnStart = time.Now()
	m.turnToolStart = time.Time{}
	m.turnToolTime = 0
	m.turnToolCalls = 0
	m.turnThinking.Reset()
}

// maxTurnThinking bounds the reasoning kept per turn. Reasoning streams are
// usually a few KB; this stops a runaway one from growing the session forever.
const maxTurnThinking = 32 << 10

// recordThinking accumulates the turn's reasoning stream for later reading.
func (m *Model) recordThinking(s string) {
	if m.turnThinking.Len() >= maxTurnThinking {
		return
	}
	m.turnThinking.WriteString(s)
}

// markToolsStart / markToolsDone bracket a batch of tool calls. Permission
// prompts sit inside this window, so approval time counts as execution time —
// that's the wall clock the user actually waited.
func (m *Model) markToolsStart(calls int) {
	m.turnToolStart = time.Now()
	m.turnToolCalls += calls
}

func (m *Model) markToolsDone() {
	if m.turnToolStart.IsZero() {
		return
	}
	m.turnToolTime += time.Since(m.turnToolStart)
	m.turnToolStart = time.Time{}
}

// finishTurnClock banks the elapsed time against the turn's user message.
func (m *Model) finishTurnClock() {
	if m.turnStart.IsZero() || m.turnAnchor < 0 {
		return
	}
	m.markToolsDone() // an interrupted turn can end mid-batch
	if m.turnRecords == nil {
		m.turnRecords = map[int]turnRecord{}
	}
	m.turnRecords[m.turnAnchor] = turnRecord{
		total:    time.Since(m.turnStart),
		tools:    m.turnToolTime,
		calls:    m.turnToolCalls,
		thinking: strings.TrimSpace(m.turnThinking.String()),
	}
	m.turnStart = time.Time{}
}

// rebaseTurnTimes shifts the keys after compaction drops the first n messages,
// so timings stay attached to their turns instead of sliding onto other ones.
func (m *Model) rebaseTurnTimes(dropped int) {
	if len(m.turnRecords) == 0 || dropped <= 0 {
		return
	}
	shifted := make(map[int]turnRecord, len(m.turnRecords))
	for k, v := range m.turnRecords {
		if k-dropped >= 0 {
			shifted[k-dropped] = v
		}
	}
	m.turnRecords = shifted
	m.turnAnchor -= dropped
}

// thinkingBlock replays a finished turn's reasoning when /show_thinking is on.
// It's indented and dimmed so it reads as an aside, not as the answer.
func (m *Model) thinkingBlock(userIdx, width int) string {
	if !m.cfg.Thinking {
		return ""
	}
	t, ok := m.turnRecords[userIdx]
	if !ok || t.thinking == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(hintStyle.Render("┌ thinking"))
	b.WriteString("\n")
	for _, line := range strings.Split(ansi.Wordwrap(t.thinking, max(width-4, 20), " -"), "\n") {
		b.WriteString(hintStyle.Render("│ " + line))
		b.WriteString("\n")
	}
	return b.String()
}

// timingFooter is the muted one-liner under a finished turn.
func (m *Model) timingFooter(userIdx int) string {
	t, ok := m.turnRecords[userIdx]
	if !ok || t.total <= 0 {
		return ""
	}
	line := "⏱ " + shortDuration(t.total)
	if t.calls > 0 {
		line += fmt.Sprintf(" · think %s · tools %s (%d call%s)",
			shortDuration(t.total-t.tools), shortDuration(t.tools), t.calls, plural(t.calls))
	}
	return mutedStyle.Render(line)
}

// shortDuration formats a turn length compactly: 840ms, 6.1s, 2m04s.
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}
