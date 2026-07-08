package tui

import (
	"encoding/json"
	"strings"

	"github.com/javanhut/ollama_code/mcp"
)

// Loop-safety tunables.
const (
	defaultMaxSteps     = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures = 2  // identical failing call attempts before short-circuit
	recentCallsKept     = 12 // fingerprint ring length for oscillation detection
)

func maxStepsFromConfig(c config) int {
	if c.MaxSteps > 0 {
		return c.MaxSteps
	}
	return defaultMaxSteps
}

// resetTurnGuards clears the per-turn loop-safety state. Call at the start of
// every new user turn (fresh submit or a dequeued message).
func (m *Model) resetTurnGuards() {
	m.stepCount = 0
	m.recentCalls = m.recentCalls[:0]
	m.oscillationWarned = false
	m.suppressToolsOnce = false
	m.lastStepTool = ""
	m.sameToolStreak = 0
	m.turnTouchedFiles = false
	m.verifyAttempts = 0
	m.challengedThisTurn = false
	for k := range m.failedCalls {
		delete(m.failedCalls, k)
	}
}

// batchSingleTool returns the tool name if every call in a batch is the same
// tool, else "". Used to detect a model spamming one tool (e.g. switch_mode)
// with varying arguments — which evades fingerprint-based repeat detection.
func batchSingleTool(calls []mcp.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	name := calls[0].Function.Name
	for _, c := range calls[1:] {
		if c.Function.Name != name {
			return ""
		}
	}
	return name
}

// canonicalJSON returns a stable serialization of a JSON value with object keys
// sorted (encoding/json marshals map keys in sorted order), so two semantically
// identical argument blobs produce the same fingerprint regardless of key order
// or whitespace. Falls back to the trimmed raw string when it isn't valid JSON.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}

// callFingerprint identifies a tool call by name + canonical arguments, so
// repeated and oscillating calls can be detected cheaply.
func callFingerprint(c mcp.ToolCall) string {
	return c.Function.Name + "\x00" + canonicalJSON(c.Function.Arguments)
}

// isOscillating reports whether the last four fingerprints form an A,B,A,B
// pattern — the model alternating between two actions without progress.
func isOscillating(recent []string) bool {
	n := len(recent)
	if n < 4 {
		return false
	}
	a, b, c, d := recent[n-4], recent[n-3], recent[n-2], recent[n-1]
	return a == c && b == d && a != b
}

// salvageJSON, repairHint, and shouldFormatRepair moved to package mcp
// (SalvageJSON/RepairHint/ShouldFormatRepair) so the headless sub-agent loop can
// reuse the same tool-call robustness. See mcp/repair.go.
