package agent

import (
	"encoding/json"
	"slices"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// Context management shared by every loop: when to mask, when to summarize,
// which tool results to stub, and how much history fits a budget. Pure
// functions over a message slice and a token estimator, so the TUI (with its
// per-model calibrated estimator) and headless runs make the same decisions.

// KeepIntactToolResults is how many of the newest tool results survive
// masking untouched: the model is actively reasoning about the round it just
// ran, and this covers two full parallel batches with room to spare.
const KeepIntactToolResults = 8

// ContextThresholds returns the masking and summarization triggers as a
// percentage of the context window. Models degrade well before their nominal
// window (Chroma "context rot", 2025; NoLiMa measured an ~1K effective length
// for an 8B model on its probes), so small models get earlier thresholds.
// Masking is the cheap tier — JetBrains' "Complexity Trap" (2025) found it
// matches or beats LLM summarization at about half the cost — so it fires
// well before summarization. Overrides of 0 mean "tier default"; the result
// is capped at 95% and masking never fires after summarization.
func ContextThresholds(small bool, maskOverride, compactOverride int) (maskPct, compactPct int) {
	maskPct, compactPct = 60, 80
	if small {
		maskPct, compactPct = 40, 70
	}
	if maskOverride > 0 {
		maskPct = min(maskOverride, 95)
	}
	if compactOverride > 0 {
		compactPct = min(max(compactOverride, maskPct), 95)
	}
	return maskPct, compactPct
}

// MaskBoundary returns the index before which tool results should be stubbed:
// everything older than the newest keepIntact tool results. 0 = nothing to mask.
func MaskBoundary(history []api.Message, keepIntact int) int {
	kept := 0
	for i, v := range slices.Backward(history) {
		if v.Role != "tool" {
			continue
		}
		kept++
		if kept > keepIntact {
			return i + 1
		}
	}
	return 0
}

// StubToolResult is the masked form of an aged tool result: the envelope's
// ok+summary, the spill path when the full output was saved to disk, and a
// hint saying how to get the detail back. Content that is not an envelope, or
// that would not shrink, is returned unchanged — better a large result than a
// mangled one.
func StubToolResult(content string) string {
	env, ok := tools.DecodeToolResult(content)
	if !ok || (len(env.Evidence) == 0 && len(env.Data) == 0 && env.Hint == "") {
		return content
	}
	hint := "older output elided to save context; re-run the call if you need the detail"
	if env.SpillPath != "" {
		hint = "older output elided to save context; read spill_path if you need the detail"
	}
	stub, err := json.Marshal(tools.ResultEnvelope{
		OK: env.OK, Summary: env.Summary, Truncated: true, SpillPath: env.SpillPath, Hint: hint,
	})
	if err != nil || len(stub) >= len(content) {
		return content
	}
	return string(stub)
}

// HistoryWindow returns the index at which the newest-fitting slice of
// history begins for a token budget: whole messages newest-first until the
// budget is spent (always keeping the most recent one), then the cut is moved
// back past any leading tool messages so a result is never sent without the
// assistant call that produced it.
func HistoryWindow(history []api.Message, budget int, estimate func(api.Message) int) int {
	remaining := budget
	start := len(history)
	for i, h := range slices.Backward(history) {
		cost := estimate(h)
		if remaining-cost < 0 && start < len(history) {
			break
		}
		remaining -= cost
		start = i
	}
	for start > 0 && history[start].Role == "tool" {
		start--
	}
	return start
}
