package tui

import "github.com/javanhut/ollama_code/api"

// estimateTokens approximates the token count of a string using the common
// ~4-chars-per-token heuristic. It is intentionally cheap so the prompt can be
// budgeted before sending, without waiting on the model's own prompt_eval count.
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// estimateMsgTokens approximates the tokens a single chat message contributes,
// including a small per-message overhead for role/formatting and any tool calls.
func estimateMsgTokens(m api.Message) int {
	n := estimateTokens(m.Content) + 4
	n += estimateTokens(m.ToolName)
	for _, c := range m.ToolCalls {
		n += estimateTokens(c.Function.Name) + estimateTokens(string(c.Function.Arguments)) + 4
	}
	return n
}

// estimateMsgsTokens sums the estimated tokens of a message slice.
func estimateMsgsTokens(msgs []api.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateMsgTokens(m)
	}
	return total
}
