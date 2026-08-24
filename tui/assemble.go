package tui

import (
	"encoding/json"
	"fmt"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// generationReserve is how many tokens we hold back from num_ctx for the model's
// own output when no explicit num_predict is set.
const generationReserve = 4096

// assembleMessages builds the final message list sent to the model under a hard
// token ceiling derived from the active context limit. Ordering is:
//
//	[static systemPrompt] -> newest-fitting history -> [volatile dynamic tail]
//
// History is included newest-first until the budget is exhausted, then emitted
// oldest-first. Whole messages are kept — a tool-result message is never sent
// without the assistant tool-call that produced it (the cut is nudged back past
// any leading "tool" messages). The static prefix stays append-only so the KV
// cache prefix remains stable across turns.
func (m *Model) assembleMessages(ragBlock string) []api.Message {
	reserve := generationReserve
	if m.profile.NumPredict != nil && *m.profile.NumPredict > 0 {
		reserve = *m.profile.NumPredict
	}
	budget := m.contextLimit - reserve
	if budget < 0 {
		budget = m.contextLimit / 2
	}

	sys := api.Message{Role: "system", Content: m.activeSystemPrompt()}
	dyn := api.Message{Role: "system", Content: m.buildDynamicContext(ragBlock)}
	base := estimateMsgTokens(sys) + estimateMsgTokens(dyn)

	visible := m.deriveModelMessages()
	start := historyWindow(visible, budget-base)
	if start > 0 {
		// This is context loss with NO archive summary behind it — compaction
		// should have run before it came to this. It shipped silent; say it out loud.
		m.toast = fmt.Sprintf("context full — dropped %d oldest messages", start)
	}

	out := make([]api.Message, 0, len(visible)-start+2)
	out = append(out, sys)
	for _, msg := range visible[start:] {
		out = append(out, demoteSystem(msg))
	}
	out = append(out, demoteSystem(dyn))
	return out
}

// demoteSystem rewrites a system message into a marked user message. Only the
// message at index 0 may carry the system role: Ollama's newer built-in
// renderers (qwen3.8 and friends) hard-fail the whole request with
// 500 "system message must be at the beginning", and the Jinja templates that
// don't fail instead reorder, merge, or silently drop it — the same reason
// advisory() in loopguard.go rides the user role.
//
// This is the one place it can be fixed once: the harness appends system
// messages into the log from a dozen call sites (dream wake context, background
// job notifications, verification and citation corrections, mode switches,
// research recipes), and assembleMessages adds the volatile dynamic tail on top,
// so every request had at least two. The [SYSTEM] marker keeps the model from
// reading harness instructions as something the user said. Only the projection
// is rewritten — the log keeps the system role for the transcript and for saved
// sessions.
func demoteSystem(msg api.Message) api.Message {
	if msg.Role != "system" {
		return msg
	}
	msg.Role = "user"
	msg.Content = "[SYSTEM] " + msg.Content
	return msg
}

// deriveModelMessages projects the model's view out of the append-only log: the
// messages after the compaction boundary, with tool results older than the
// prune boundary reduced to their envelope headline. It is the ONLY place the
// log becomes model input. Anything reading m.history directly — the
// transcript, the citation gate, the turn anchor — is reading the RECORD, which
// is a different question and must not be confused for this one.
//
// ponytail: re-derived per call rather than cached. The window is bounded by
// the context limit, so this is a few hundred pointer copies plus a stub encode
// for the pruned tail. If a profiler ever disagrees, cache it against
// len(m.history) plus the two boundaries — exactly the key that invalidates
// correctly.
func (m *Model) deriveModelMessages() []api.Message {
	start := min(m.archivedThrough, len(m.history))
	out := make([]api.Message, 0, len(m.history)-start)
	for i := start; i < len(m.history); i++ {
		msg := m.history[i]
		// Reasoning is display-and-record only, but api.Message tags it
		// `json:"thinking"` and ChatRequest.Messages reuses that same struct, so
		// an unstripped copy ships the model its own prior reasoning on every
		// request — often several times longer than the answer it belongs to,
		// and several open-weight thinking models degrade when fed it back.
		// estimateMsgTokens() doesn't count Thinking either, so leaving it in
		// makes shouldCompact/historyWindow undercount what was actually sent.
		// Cleared on the copy: m.history keeps it for the transcript,
		// /show_thinking and saved sessions.
		msg.Thinking = ""
		if msg.Role == "tool" && i < m.prunedThrough {
			msg.Content = prunedToolContent(msg.Content)
		}
		out = append(out, msg)
	}
	return out
}

// historyWindow returns the index at which the kept (newest-fitting) slice of
// history begins, given a token budget. It includes whole messages newest-first
// until the budget is exhausted (always keeping at least the most recent one),
// then nudges the cut back past any leading "tool" messages so a tool result is
// never sent without its originating assistant tool-call.
func historyWindow(history []api.Message, budget int) int {
	remaining := budget
	start := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		cost := estimateMsgTokens(history[i])
		if remaining-cost < 0 && start < len(history) {
			break // keep at least the most recent message even if oversized
		}
		remaining -= cost
		start = i
	}
	for start > 0 && history[start].Role == "tool" {
		start--
	}
	return start
}

// observePromptEval records the provider's real prompt token count for the
// request we just sent. A zero count — a provider that reports no usage, or a
// stream that closed without a done chunk — is ignored, so the last good
// measurement survives instead of being wiped back to "unmeasured".
func (m *Model) observePromptEval(n int) {
	if n <= 0 {
		return
	}
	m.prevPromptEval, m.lastPromptEval = m.lastPromptEval, n
}

// shouldCompact reports whether context pressure has crossed the proactive
// compaction threshold (80% of the limit).
//
// Apples-to-apples: prompt_eval_count is the size of the request we actually
// sent — system prompt + windowed history + the volatile dynamic tail — which
// is exactly what num_ctx bounds. The chars-per-token estimate covers m.history
// ALONE, so it omits the multi-thousand-token system prompt and the whole
// dynamic tail and reads far too low; compaction fired late and historyWindow
// then hard-dropped the oldest messages with no archive summary behind them.
// Prefer the measured count, smoothed by taking the smaller of the last two so
// one outlier turn can't fire on its own. The estimate stays the floor — it
// carries the first turns and any provider that reports no usage, and it wins
// whenever it is larger, because it sees the messages appended since the last
// measurement.
func (m *Model) shouldCompact() bool {
	if len(m.history)-m.archivedThrough < 6 {
		return false
	}
	pressure := estimateMsgsTokens(m.deriveModelMessages())
	if measured := min(m.lastPromptEval, m.prevPromptEval); measured > pressure {
		pressure = measured
	}
	return pressure > m.contextLimit*8/10
}

// requestEstimate sizes the parts of the request a compaction pass can move:
// history, plus the rolling archive summary the dropped half is replaced by.
// The system prompt and the RAG block are identical on both sides of a pass, so
// leaving them out costs nothing and keeps this off assembleMessages' work. A
// summary as long as what it replaced therefore correctly reads as no progress.
//
// Tokens only, deliberately: a message COUNT that fell is not progress a
// provider cares about, and 40 short messages replaced by one longer summary
// would pass a count check while overflowing identically.
func (m *Model) requestEstimate() int {
	return estimateMsgsTokens(m.deriveModelMessages()) + estimateTokens(m.archiveSummary)
}

// keepIntactToolResults is how many of the newest tool results survive pruning
// untouched: the model is actively reasoning about the round it just ran, and
// this covers two full parallel batches (maxParallelToolCalls tops out at 4)
// with room to spare.
const keepIntactToolResults = 8

// pruneToolResults advances the prune boundary past everything older than the
// newest keepIntactToolResults tool results. Old tool output is where a long
// session's tokens actually go, and reclaiming it costs no model round-trip.
//
// A boundary, not a rewrite: the log keeps the full result, so the transcript
// still shows what the tool said, /undo and a saved session still carry it, and
// re-pruning is idempotent by construction rather than by a marker check. The
// projection stubs; the record never loses anything. Reports whether the
// boundary moved.
func (m *Model) pruneToolResults() bool {
	boundary := 0
	kept := 0
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role != "tool" {
			continue
		}
		kept++
		if kept > keepIntactToolResults {
			boundary = i + 1
			break
		}
	}
	if boundary <= m.prunedThrough {
		return false
	}
	m.prunedThrough = boundary
	return true
}

// prunedToolContent is the projected form of an aged tool result: the
// envelope's ok+summary, plus the spill path when the full output was saved to
// disk, so the model still knows the call happened, how it went, and where to
// read the detail back from. Content it cannot decode as an envelope is passed
// through — better a large unpruned result than a mangled one.
func prunedToolContent(content string) string {
	env, ok := tools.DecodeToolResult(content)
	if !ok || (len(env.Evidence) == 0 && len(env.Data) == 0 && env.Hint == "") {
		return content
	}
	stub, err := json.Marshal(tools.ResultEnvelope{
		OK: env.OK, Summary: env.Summary, Truncated: true, SpillPath: env.SpillPath,
	})
	if err != nil || len(stub) >= len(content) {
		return content
	}
	return string(stub)
}
