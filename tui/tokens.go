package tui

import (
	"sync"

	"github.com/javanhut/ollama_code/api"
)

// defaultCharsPerToken is the fallback heuristic for token estimation, used
// until /model calibrate measures the model's real ratio.
const defaultCharsPerToken = 4.0

// ratioState guards the configured chars-per-token ratio: calibration installs
// it from a background command goroutine while the estimators read it from the
// render loop. key is the provider|model the ratio was loaded for, so the
// persisted value is read from disk at most once per model switch.
var ratioState = struct {
	sync.RWMutex
	charsPerToken float64
	key           string
}{charsPerToken: defaultCharsPerToken}

// SetCharsPerToken installs a measured chars-per-token ratio (from
// /model calibrate). A non-positive value resets to the default heuristic;
// values outside [1.5, 8] are rejected as garbage counts.
func SetCharsPerToken(r float64) {
	if r <= 0 {
		r = defaultCharsPerToken
	} else if r < 1.5 || r > 8 {
		return
	}
	ratioState.Lock()
	ratioState.charsPerToken = r
	ratioState.Unlock()
}

// CharsPerToken returns the active ratio — the measured one when calibration
// has provided it, otherwise the 4.0 heuristic.
func CharsPerToken() float64 {
	ratioState.RLock()
	defer ratioState.RUnlock()
	return ratioState.charsPerToken
}

// ratioKeySeen reports whether key matches the model the ratio was loaded for.
func ratioKeySeen(key string) bool {
	ratioState.RLock()
	defer ratioState.RUnlock()
	return ratioState.key == key
}

// markRatioKey records which model the current ratio belongs to.
func markRatioKey(key string) {
	ratioState.Lock()
	ratioState.key = key
	ratioState.Unlock()
}

// estimateTokens approximates the token count of a string using the active
// chars-per-token ratio — measured by /model calibrate when available, the
// common ~4-chars-per-token heuristic otherwise. It is intentionally cheap so
// the prompt can be budgeted before sending, without waiting on the model's
// own prompt_eval count.
func estimateTokens(s string) int {
	return estimateTokensLen(len(s))
}

// estimateTokensLen is estimateTokens for a known length, avoiding a string
// copy when only the size is at hand (e.g. a strings.Builder).
func estimateTokensLen(n int) int {
	r := CharsPerToken()
	return int((float64(n) + r - 1) / r)
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

// displayTokens returns the token count the ctx meter should show.
// m.totalTokens only updates when a stream completes, so mid-turn it is a
// turn stale: while a stream (or its tool calls) is in flight, take the larger
// of that count and a live estimate of the history — which grows as assistant
// rounds and tool results land — plus the partial reply buffered so far.
func (m *Model) displayTokens() int {
	n := m.totalTokens
	if !m.streaming && m.pending == nil {
		return n
	}
	est := estimateMsgsTokens(m.history)
	if m.streamBuf != nil {
		est += estimateTokensLen(m.streamBuf.Len())
	}
	if est > n {
		n = est
	}
	return n
}
