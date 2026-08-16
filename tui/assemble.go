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

	start := historyWindow(m.history, budget-base)
	if start > 0 {
		// This is context loss with NO archive summary behind it — compaction
		// should have run before it came to this. It shipped silent; say it out loud.
		m.toast = fmt.Sprintf("context full — dropped %d oldest messages", start)
	}

	out := make([]api.Message, 0, len(m.history)-start+2)
	out = append(out, sys)
	out = append(out, m.history[start:]...)
	out = append(out, dyn)
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
	if len(m.history) < 6 {
		return false
	}
	pressure := estimateMsgsTokens(m.history)
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
	return estimateMsgsTokens(m.history) + estimateTokens(m.archiveSummary)
}

// keepIntactToolResults is how many of the newest tool results survive pruning
// untouched: the model is actively reasoning about the round it just ran, and
// this covers two full parallel batches (maxParallelToolCalls tops out at 4)
// with room to spare.
const keepIntactToolResults = 8

// pruneToolResults shrinks the BODIES of old tool-result messages in place,
// leaving the envelope's ok+summary (and the spill path, if the output was
// saved) as the one-line stub: the model still knows the call happened and how
// it went. Old tool output is where a long session's tokens actually go, and
// reclaiming it costs no model round-trip.
//
// Nothing is added, removed or reordered and ToolName/ToolCalls are untouched,
// so the assistant-call->result pairing, historyWindow's leading-"tool" nudge,
// the OpenAI adapter's positional tool_call_id correlation and the index-keyed
// turn timings all keep working with no extra bookkeeping. Dropping
// evidence/data/hint outright (rather than writing a marker) is what makes a
// second pass a no-op instead of nesting stubs. Returns how many it pruned.
func pruneToolResults(history []api.Message) int {
	pruned, kept := 0, 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "tool" {
			continue
		}
		if kept < keepIntactToolResults {
			kept++
			continue
		}
		env, ok := tools.DecodeToolResult(history[i].Content)
		if !ok || (len(env.Evidence) == 0 && len(env.Data) == 0 && env.Hint == "") {
			continue // not an envelope, or already pruned — nothing left to drop
		}
		stub, err := json.Marshal(tools.ResultEnvelope{
			OK: env.OK, Summary: env.Summary, Truncated: true, SpillPath: env.SpillPath,
		})
		if err != nil || len(stub) >= len(history[i].Content) {
			continue
		}
		history[i].Content = string(stub)
		pruned++
	}
	return pruned
}
