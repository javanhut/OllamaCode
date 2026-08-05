package tui

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func (m *Model) refreshTranscript() {
	var b strings.Builder
	if len(m.history) == 0 && !m.streaming && m.lastError == "" {
		// With the welcome panel off, an empty session rendered as a blank void.
		if m.welcomeOn() {
			b.WriteString(m.welcomePanel())
		} else {
			b.WriteString(m.emptyState())
		}
	} else {
		// Non-empty history (or welcome suppressed → the loop below is a no-op).
		// Group consecutive assistant + tool messages into a single Layla
		// turn so the user sees one block per response, not one per tool call.
		i := 0
		userIdx := -1 // the command each turn is answering, for its timing footer
		var openTurn *assistantTurn
		flushTurn := func() {
			if openTurn != nil {
				m.writeAssistantTurn(&b, openTurn, false)
				openTurn = nil
			}
		}
		for i < len(m.history) {
			msg := m.history[i]
			switch msg.Role {
			case "user":
				flushTurn()
				userIdx = i
				b.WriteString(userStyle.Render("You"))
				b.WriteString("\n")
				// Strip escapes/CR/control chars so pasted content can't
				// corrupt the terminal layout.
				b.WriteString(stripControl(msg.Content))
				b.WriteString("\n\n")
				i++
			case "assistant", "tool":
				turn, next := m.collectAssistantTurn(i)
				turn.userIdx = userIdx
				// A turn with something after it can never change again, so render
				// it once and reuse the string. Without this every frame re-ran the
				// tool grouping and re-colorized every diff in the whole session —
				// on a 60ms streaming cadence.
				if next < len(m.history) {
					b.WriteString(m.cachedTurn(&turn, i, next))
					i = next
					continue
				}
				openTurn = &turn
				i = next
			default:
				flushTurn()
				if msg.Content != "" {
					b.WriteString(m.renderMarkdown(msg.Content, true))
					b.WriteString("\n\n")
				}
				i++
			}
		}

		switch {
		case m.streaming:
			if openTurn == nil {
				openTurn = &assistantTurn{userIdx: userIdx}
			}
			openTurn.streaming = true
			if m.streamBuf.Len() > 0 {
				openTurn.segments = append(openTurn.segments, turnSegment{text: m.streamBuf.String()})
			}
		case m.retrieving || m.compacting || m.verifying:
			// Turn-start gates (RAG retrieval, compaction) and the verify gate
			// run with m.streaming false; open a turn anyway so the phase
			// spinner renders instead of the transcript looking frozen.
			if openTurn == nil {
				openTurn = &assistantTurn{userIdx: userIdx}
			}
			openTurn.streaming = true
		}
		flushTurn()
	}
	// ... rest of method ...
	if m.lastError != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.lastError))
		b.WriteString("\n")
	}

	content := b.String()
	if content != m.transcript.String() {
		m.transcript.Reset()
		m.transcript.WriteString(content)
		m.viewport.SetContent(content)
	}

	if m.sel.active {
		m.applySelectionHighlight()
	}
}

// turnCacheLimit keeps the cache from growing without bound in a long session.
// Entries are cheap (one rendered turn each); this just puts a ceiling on it.
const turnCacheLimit = 256

// cachedTurn renders a sealed turn, reusing the previous render when nothing
// that affects it has changed. The key is a hash of the turn's own messages, so
// anything that rewrites history — compaction, /clear, /load, undo — misses
// naturally instead of needing every mutation site to invalidate by hand.
func (m *Model) cachedTurn(t *assistantTurn, start, next int) string {
	stamp := fmt.Sprintf("%d|%t|%t|%t|%s|%s",
		m.viewport.Width(), m.expandTools, m.cfg.Verbose, m.cfg.Thinking, m.mode, m.activeModelName())
	if stamp != m.turnCacheStamp || len(m.turnCache) > turnCacheLimit {
		m.turnCache = make(map[uint64]string)
		m.turnCacheStamp = stamp
	}

	h := fnv.New64a()
	fmt.Fprintf(h, "%d|", t.userIdx)
	if r, ok := m.turnRecords[t.userIdx]; ok {
		fmt.Fprintf(h, "%d|%d|%d|%d|", r.total, r.tools, r.calls, len(r.thinking))
	}
	for _, msg := range m.history[start:next] {
		h.Write([]byte(msg.Role))
		h.Write([]byte(msg.ToolName))
		h.Write([]byte(msg.Content))
		for _, c := range msg.ToolCalls {
			h.Write([]byte(c.Function.Name))
			h.Write(c.Function.Arguments)
		}
	}
	key := h.Sum64()
	if out, ok := m.turnCache[key]; ok {
		return out
	}
	var tb strings.Builder
	m.writeAssistantTurn(&tb, t, false)
	out := tb.String()
	m.turnCache[key] = out
	return out
}

// turnSegment is one ordered piece of an assistant turn: either a block of
// assistant text or a single visible tool call, in the order they happened.
type turnSegment struct {
	text string         // non-empty => text segment
	tool *toolCallEntry // non-nil => tool-call segment
}

// assistantTurn is one rendered Layla block: all assistant content between
// two user messages, plus the visible tool calls fired during it, kept in
// chronological order.
type assistantTurn struct {
	segments  []turnSegment
	streaming bool
	userIdx   int // history index of the user message this turn answers; -1 if none
}

type toolCallEntry struct {
	call      tools.ToolCall
	result    string
	hasResult bool
}

// collectAssistantTurn walks history starting at i, gathering every
// contiguous assistant/tool message into a single turn. Returns the turn and
// the index of the next non-assistant/non-tool message (or len(history)).
func (m *Model) collectAssistantTurn(start int) (assistantTurn, int) {
	var t assistantTurn
	consumed := make(map[int]bool)
	i := start
	for i < len(m.history) {
		msg := m.history[i]
		if msg.Role != "assistant" && msg.Role != "tool" {
			break
		}
		if msg.Role == "assistant" {
			if msg.Content != "" {
				t.segments = append(t.segments, turnSegment{text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				resultIdx := -1
				for j := i + 1; j < len(m.history); j++ {
					if m.history[j].Role != "tool" && m.history[j].Role != "assistant" {
						break
					}
					if !consumed[j] && m.history[j].Role == "tool" && m.history[j].ToolName == call.Function.Name {
						resultIdx = j
						consumed[j] = true
						break
					}
				}
				if invisibleTools[call.Function.Name] {
					continue
				}
				entry := toolCallEntry{call: call}
				if resultIdx >= 0 {
					entry.result = m.history[resultIdx].Content
					entry.hasResult = true
				}
				t.segments = append(t.segments, turnSegment{tool: &entry})
			}
		}
		i++
	}
	return t, i
}

// writeAssistantTurn renders a turn as a single Layla block: header, then the
// turn's segments in chronological order — text where it was produced, tool
// calls where they fired. Tool calls are collapsed by default, expanded when
// the user has toggled `ctrl+t`; consecutive calls group into one summary.
func (m *Model) writeAssistantTurn(b *strings.Builder, t *assistantTurn, _ bool) {
	b.WriteString(assistantStyle.Foreground(m.mode.color()).Render(m.activeModelName()))
	b.WriteString("\n")

	// The reasoning that produced this answer, when /show_thinking is on. It ran
	// before the answer, so it reads first.
	if !t.streaming {
		b.WriteString(m.thinkingBlock(t.userIdx, m.viewport.Width()))
	}

	hasText := false
	for _, seg := range t.segments {
		if seg.text != "" {
			hasText = true
			break
		}
	}

	if t.streaming && !hasText && m.pending == nil {
		phase := " Thinking..."
		switch {
		case m.retrieving:
			phase = " Searching code..."
		case m.compacting:
			phase = " Compacting context..."
		case m.verifying:
			phase = " Verifying..."
		}
		b.WriteString(m.spinner.View())
		b.WriteString(mutedStyle.Render(phase))
		b.WriteString("\n")
		// Live reasoning ticker: the last line of the model's thinking stream,
		// so long reasoning reads as progress instead of a frozen spinner.
		if line := lastNonEmptyLine(m.thinkTail); line != "" {
			b.WriteString(mutedStyle.Render("  " + truncatePlain(line, max(m.viewport.Width()-4, 20))))
			b.WriteString("\n")
		}
	}

	toolWidth := max(m.viewport.Width()-4, 20)
	for i := 0; i < len(t.segments); i++ {
		seg := t.segments[i]
		if seg.tool == nil {
			if t.streaming {
				// Rendering incomplete Markdown makes the layout jump whenever an
				// unfinished fence/list/table becomes valid, and rerunning Glamour
				// over the growing answer every frame is quadratic work. Stream
				// stable sanitized text; render the final Markdown once at completion.
				b.WriteString(stripControl(seg.text))
			} else {
				b.WriteString(m.renderMarkdown(seg.text, true))
			}
			b.WriteString("\n")
			continue
		}
		// Group the run of consecutive tool-call segments.
		j := i
		for j < len(t.segments) && t.segments[j].tool != nil {
			j++
		}
		group := t.segments[i:j]
		i = j - 1

		b.WriteString("\n")
		if m.expandTools {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("▾ %d tool call%s (ctrl+t to collapse)",
				len(group), plural(len(group)))))
			b.WriteString("\n")
			for _, s := range group {
				entry := s.tool
				if entry.hasResult {
					// When a mutating tool reports a diff, show it colorized (green
					// additions / red deletions) below the header instead of a plain
					// dump — so edits read like any other coding CLI.
					if summary, diff := splitDiff(entry.result); diff != "" {
						b.WriteString(m.renderMarkdown(renderCollapsedTool(entry.call, summary, m.cfg.Verbose, toolWidth), true))
						b.WriteString("\n")
						b.WriteString(colorizeDiff(diff, m.viewport.Width()-2))
					} else {
						b.WriteString(m.renderMarkdown(renderCollapsedTool(entry.call, entry.result, m.cfg.Verbose, toolWidth), true))
					}
				} else {
					b.WriteString(m.renderMarkdown(renderToolCall(entry.call, m.cfg.Verbose, toolWidth), true))
				}
				b.WriteString("\n")
			}
		} else {
			names := make([]string, 0, len(group))
			for _, s := range group {
				names = append(names, s.tool.call.Function.Name)
			}
			summary := fmt.Sprintf("▸ %d tool call%s — %s · ctrl+t to expand",
				len(group), plural(len(group)), strings.Join(uniqueNames(names), ", "))
			b.WriteString(mutedStyle.Render(summary))
			b.WriteString("\n")
			// Even when tool calls are collapsed, always surface file diffs so the
			// user sees the changes the agent made — like any other coding CLI.
			for _, s := range group {
				entry := s.tool
				if _, diff := splitDiff(entry.result); diff != "" {
					b.WriteString(mutedStyle.Render("  ✎ " + entry.call.Function.Name))
					b.WriteString("\n")
					b.WriteString(colorizeDiff(diff, m.viewport.Width()-2))
					b.WriteString("\n")
				}
			}
		}
	}

	if t.streaming && m.pending != nil && m.pending.done < len(m.pending.calls) {
		b.WriteString(m.spinner.View())
		label := m.currentToolLabel()
		if label != "" {
			b.WriteString(mutedStyle.Render(fmt.Sprintf(" running %s… (%d/%d)%s", label, m.pending.done, len(m.pending.calls), m.elapsedSuffix())))
		} else {
			b.WriteString(mutedStyle.Render(" working…" + m.elapsedSuffix()))
		}
		b.WriteString("\n")
	}
	if t.streaming && m.verifying {
		b.WriteString(m.spinner.View())
		b.WriteString(mutedStyle.Render(" verifying…" + m.elapsedSuffix()))
		b.WriteString("\n")
	}
	// What this command cost, once it's done. Live turns already show a ticking
	// elapsed counter in the phase line above.
	if !t.streaming {
		if footer := m.timingFooter(t.userIdx); footer != "" {
			b.WriteString(footer)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
}

func (m *Model) lastTurnDiffs() string {
	var diffs []string
	for i := len(m.history) - 1; i >= 0; i-- {
		msg := m.history[i]
		if msg.Role == "user" {
			break
		}
		if msg.Role == "tool" {
			if _, d := splitDiff(msg.Content); d != "" {
				diffs = append(diffs, d)
			}
		}
	}
	for l, r := 0, len(diffs)-1; l < r; l, r = l+1, r-1 {
		diffs[l], diffs[r] = diffs[r], diffs[l]
	}
	return strings.Join(diffs, "\n\n")
}

func (m *Model) lastUserMessage() string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "user" {
			s := m.history[i].Content
			if len(s) > 48 {
				s = s[:48] + "…"
			}
			return s
		}
	}
	return "last turn"
}

func lastAssistantMessage(history []api.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			return history[i].Content
		}
	}
	return ""
}
