package tui

import (
	"fmt"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
)

// generationReserve is how many tokens we hold back from num_ctx for the model's
// own output when no explicit num_predict is set.
const generationReserve = 4096

// assembleMessages builds the final message list sent to the model under a hard
// token ceiling derived from the active context limit. Ordering is:
//
//	[static systemPrompt] -> newest-fitting history -> [dynamic tail]
//
// The tail is the whole dynamic block, or — under cfg.ContextDelta — only its
// volatile half, the stable half having been appended into history by
// reconcileContext as a durable message that is re-sent only when it changes.
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
	// Under ContextDelta the stable half of the block rides history as durable
	// messages and only the volatile tail is re-sent. reconcileContext appends to
	// m.history, so it has to run before deriveModelMessages projects it below.
	var tail string
	if m.cfg.ContextDelta {
		m.reconcileContext()
		tail = m.volatileContext(ragBlock)
	} else {
		tail = m.buildDynamicContext(ragBlock)
	}
	dyn := api.Message{Role: "system", Content: tail}
	base := estimateMsgTokens(sys) + estimateMsgTokens(dyn)

	visible := m.deriveModelMessages()
	start := historyWindow(visible, budget-base)
	if start > 0 && m.cfg.ContextDelta {
		// The window is evicting messages, and the durable baseline may be among
		// them — so this request cannot lean on history to carry the stable half.
		// Fall back to the whole block in the tail, for THIS request only.
		//
		// Clearing contextSnapshot here instead, which is what this used to do,
		// is a positive feedback loop: the next assembly re-appends a full
		// baseline into history, which evicts more messages, which keeps the drop
		// firing. Eleven rounds of it and the window held seven identical
		// [CONTEXT UPDATE] copies and not one conversation message. The snapshot
		// stays as it is: nothing was lost that the tail is not now carrying.
		dyn = api.Message{Role: "system", Content: m.buildDynamicContext(ragBlock)}
		base = estimateMsgTokens(sys) + estimateMsgTokens(dyn)
		start = historyWindow(visible, budget-base)
	}
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

// historyWindow: see agent.HistoryWindow, with the calibrated estimator.
func historyWindow(history []api.Message, budget int) int {
	return agent.HistoryWindow(history, budget, estimateMsgTokens)
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
	// Ollama truncates an over-long prompt to num_ctx without an error, and it
	// cuts from the front, i.e. the system prompt. A request that measured at
	// the window's edge was probably cut; say so rather than letting the model
	// behave mysteriously without its instructions.
	if m.contextLimit > 0 && n >= m.contextLimit*98/100 {
		m.toast = fmt.Sprintf("prompt hit the context window (%d/%d tokens) — the provider may have truncated it; consider a larger num_ctx", n, m.contextLimit)
	}
}

// contextPressure is the best available size of the request we send.
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
func (m *Model) contextPressure() int {
	pressure := estimateMsgsTokens(m.deriveModelMessages())
	if measured := min(m.lastPromptEval, m.prevPromptEval); measured > pressure {
		pressure = measured
	}
	return pressure
}

// shouldCompact reports whether context pressure has crossed the proactive
// summarization threshold (profile compactAtPct of the limit; 80% by default,
// 70% for small models).
func (m *Model) shouldCompact() bool {
	if len(m.history)-m.archivedThrough < 6 {
		return false
	}
	return m.contextPressure() > m.contextLimit*m.profile.compactAtPct()/100
}

// shouldMask reports whether pressure has crossed the masking threshold
// (profile maskAtPct), at which old tool output is stubbed without a model
// call. Below the summarization threshold by construction.
func (m *Model) shouldMask() bool {
	return m.contextLimit > 0 && m.contextPressure() > m.contextLimit*m.profile.maskAtPct()/100
}

// maskOldToolResults advances the prune boundary and keeps the measured
// counts honest. The measured counts describe the PRE-prune request, but
// zeroing them hands the decision back to the estimate — which, whenever the
// measurement is what fired this pass, is BY DEFINITION below the threshold. A
// 43-token prune would then cancel a 28k/32k compaction, and keep cancelling it
// every time, while the real context grew to the provider's hard refusal.
// Subtract what pruning actually reclaimed and let the measurement stay
// authoritative. Reports whether anything was masked.
func (m *Model) maskOldToolResults() bool {
	before := estimateMsgsTokens(m.deriveModelMessages())
	if !m.pruneToolResults() {
		return false
	}
	reclaimed := before - estimateMsgsTokens(m.deriveModelMessages())
	m.lastPromptEval = max(0, m.lastPromptEval-reclaimed)
	m.prevPromptEval = max(0, m.prevPromptEval-reclaimed)
	return true
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

// keepIntactToolResults: see agent.KeepIntactToolResults.
const keepIntactToolResults = agent.KeepIntactToolResults

// pruneToolResults advances the mask boundary (agent.MaskBoundary). A
// boundary, not a rewrite: the log keeps the full result, so the transcript,
// /undo and saved sessions still carry it, and re-masking is idempotent. The
// projection stubs; the record never loses anything. Reports whether the
// boundary moved.
func (m *Model) pruneToolResults() bool {
	boundary := agent.MaskBoundary(m.history, keepIntactToolResults)
	if boundary <= m.prunedThrough {
		return false
	}
	m.prunedThrough = boundary
	return true
}

// prunedToolContent: see agent.StubToolResult.
func prunedToolContent(content string) string { return agent.StubToolResult(content) }
