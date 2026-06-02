package tui

import "github.com/javanhut/ollama_code/api"

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

	sys := api.Message{Role: "system", Content: systemPrompt}
	dyn := api.Message{Role: "system", Content: m.buildDynamicContext(ragBlock)}
	base := estimateMsgTokens(sys) + estimateMsgTokens(dyn)

	start := historyWindow(m.history, budget-base)

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

// shouldCompact reports whether the estimated history size has crossed the
// proactive compaction threshold (80% of budget). It uses the cheap char/4
// estimate so we can decide before paying for a model round-trip.
func (m *Model) shouldCompact() bool {
	if len(m.history) < 6 {
		return false
	}
	return estimateMsgsTokens(m.history) > m.contextLimit*8/10
}
