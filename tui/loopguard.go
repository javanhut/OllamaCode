package tui

import (
	"github.com/javanhut/ollama_code/mcp"
)

// Loop-safety tunables.
const (
	defaultMaxSteps     = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures = 2  // identical failing call attempts before short-circuit
	recentCallsKept     = 12 // fingerprint ring length for oscillation detection
	maxAutoContinues    = 3  // times we nudge the model to keep going on open todos before yielding
	maxStreamRetries    = 2  // transient stream errors auto-retried per turn before surfacing
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
	m.streamRetries = 0
	m.recentCalls = m.recentCalls[:0]
	m.oscillationWarned = false
	m.suppressToolsOnce = false
	m.lastStepTool = ""
	m.sameToolStreak = 0
	m.turnTouchedFiles = false
	m.verifyAttempts = 0
	m.challengedThisTurn = false
	m.autoContinues = 0
	for k := range m.failedCalls {
		delete(m.failedCalls, k)
	}
}

// dedupeCalls: see mcp.DedupeCalls (shared with the headless sub-agent loop).
func dedupeCalls(calls []mcp.ToolCall) []mcp.ToolCall { return mcp.DedupeCalls(calls) }

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

// canonicalJSON/callFingerprint/isOscillating moved to package mcp
// (CanonicalJSON/CallFingerprint/IsOscillating), and salvageJSON/repairHint/
// shouldFormatRepair to mcp too (SalvageJSON/RepairHint/ShouldFormatRepair), so
// the headless sub-agent loop reuses the same tool-call safety. See
// mcp/loopguard.go and mcp/repair.go.
