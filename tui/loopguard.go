package tui

import (
	"sort"
	"strings"

	"github.com/javanhut/ollama_code/tools"
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
	m.lastStepRepeatKey = ""
	m.sameToolStreak = 0
	m.sameToolWarned = false
	m.sameToolStopWarned = false
	m.turnTouchedFiles = false
	m.verifyAttempts = 0
	m.challengedThisTurn = false
	m.autoContinues = 0
	for k := range m.failedCalls {
		delete(m.failedCalls, k)
	}
}

// dedupeCalls: see tools.DedupeCalls (shared with the headless sub-agent loop).
func dedupeCalls(calls []tools.ToolCall) []tools.ToolCall { return tools.DedupeCalls(calls) }

// batchSingleTool returns the tool name if every call in a batch is the same
// tool, else "". Used to detect a model spamming one tool (e.g. switch_mode)
// with varying arguments — which evades fingerprint-based repeat detection.
func batchSingleTool(calls []tools.ToolCall) string {
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

// argumentSensitiveRepeatTools are naturally iterative inspection operations.
// Different arguments mean the model is gathering different information, not
// repeating an action. Exact repeats are still guarded.
var argumentSensitiveRepeatTools = map[string]bool{
	"read_file": true, "list_directory": true, "find_files": true,
	"grep": true, "file_info": true, "find_symbol": true,
	"code_definition": true, "code_references": true, "code_hover": true,
	"semantic_search": true, "git_diff": true, "git_log": true,
}

// batchRepeatIdentity returns the display tool name and semantic repeat key for
// a single-tool batch. Mixed batches are progress and return empty identities.
func batchRepeatIdentity(calls []tools.ToolCall) (string, string) {
	tool := batchSingleTool(calls)
	if tool == "" {
		return "", ""
	}
	if !argumentSensitiveRepeatTools[tool] {
		return tool, tool
	}
	fingerprints := make([]string, len(calls))
	for i, call := range calls {
		fingerprints[i] = tools.CallFingerprint(call)
	}
	// Reordering the same parallel reads is still the same action.
	sort.Strings(fingerprints)
	return tool, strings.Join(fingerprints, "\x01")
}

// observeRepeatedBatch advances the per-turn repetition state. warn is emitted
// at most once per user turn. stop remains true after the hard threshold so a
// text-form tool call cannot bypass a single tool-less response; announceStop
// keeps the transcript explanation to one copy.
func (m *Model) observeRepeatedBatch(calls []tools.ToolCall) (tool string, warn, stop, announceStop bool) {
	tool, key := batchRepeatIdentity(calls)
	if key == "" {
		m.lastStepRepeatKey = ""
		m.sameToolStreak = 0
		return "", false, false, false
	}
	if key == m.lastStepRepeatKey {
		m.sameToolStreak++
	} else {
		m.lastStepRepeatKey = key
		m.sameToolStreak = 1
	}
	if m.sameToolStreak >= 3 && !m.sameToolWarned {
		m.sameToolWarned = true
		warn = true
	}
	if m.sameToolStreak >= 5 {
		stop = true
		if !m.sameToolStopWarned {
			m.sameToolStopWarned = true
			announceStop = true
		}
	}
	return tool, warn, stop, announceStop
}

// canonicalJSON/callFingerprint/isOscillating moved to package tools
// (CanonicalJSON/CallFingerprint/IsOscillating), and salvageJSON/repairHint/
// shouldFormatRepair to tools too (SalvageJSON/RepairHint/ShouldFormatRepair), so
// the headless sub-agent loop reuses the same tool-call safety. See
// tools/loopguard.go and tools/repair.go.
