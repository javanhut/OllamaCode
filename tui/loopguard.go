package tui

import (
	"encoding/json"
	"path/filepath"
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
	m.startTurnClock()
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
	for k := range m.turnReads {
		delete(m.turnReads, k)
	}
	m.planNeedsVerify = false
	m.planPaths = nil
	m.rereadEvents = 0
	m.rereadStopAnnounced = false
	m.lastPreamble = ""
	m.preambleStreak = 0
	m.preambleWarned = false
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

// maxRereadEvents caps how many times a turn may re-read unchanged files
// before tools are suppressed and the model is forced to answer in text.
const maxRereadEvents = 4

// pathKeyedReadTools return the same bytes for the same target, so re-reading
// one without an intervening edit is always wasted work. grep and find_files
// are excluded on purpose: same path with a different pattern is a different
// question, not a repeat.
var pathKeyedReadTools = map[string]bool{
	"read_file": true, "list_directory": true, "file_info": true,
}

// readTargetKey returns a stable identity for a path-keyed read call.
func readTargetKey(call tools.ToolCall) (string, bool) {
	if !pathKeyedReadTools[call.Function.Name] {
		return "", false
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Function.Arguments, &a); err != nil || a.Path == "" {
		return "", false
	}
	return call.Function.Name + "\x01" + filepath.Clean(a.Path), true
}

// observeFileReads records path-keyed reads for the turn and reports targets
// the model has already read without an intervening mutation. The streak
// guard misses these because it resets whenever any other call interleaves.
func (m *Model) observeFileReads(calls []tools.ToolCall) (rereads []string, stop bool) {
	if m.turnReads == nil {
		m.turnReads = map[string]int{}
	}
	seen := map[string]bool{}
	for _, call := range calls {
		key, ok := readTargetKey(call)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		m.turnReads[key]++
		if m.turnReads[key] > 1 {
			rereads = append(rereads, strings.SplitN(key, "\x01", 2)[1])
			m.rereadEvents++
		}
	}
	return rereads, m.rereadEvents >= maxRereadEvents
}

// forgetReads drops read records for paths a mutating tool just changed, so
// re-reading a file after editing it is never treated as a loop.
func (m *Model) forgetReads(paths []string) {
	for _, p := range paths {
		clean := filepath.Clean(p)
		for tool := range pathKeyedReadTools {
			delete(m.turnReads, tool+"\x01"+clean)
		}
	}
}

// minPreambleLen skips trivially short preambles ("OK", "Sure") — two of
// those in a row is a style choice, not an echo loop.
const minPreambleLen = 24

// normalizePreamble collapses case and whitespace so lightly reworded echoes
// compare equal.
func normalizePreamble(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// similarPreamble reports whether two assistant messages are the same thought
// restated: identical after normalization, one containing the other, or heavy
// word overlap (Jaccard >= 0.6).
func similarPreamble(a, b string) bool {
	a, b = normalizePreamble(a), normalizePreamble(b)
	if a == "" || b == "" {
		return false
	}
	if a == b || strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	setA := map[string]bool{}
	for _, w := range strings.Fields(a) {
		setA[w] = true
	}
	shared, union := 0, len(setA)
	for _, w := range strings.Fields(b) {
		if setA[w] {
			shared++
		} else {
			union++
		}
	}
	return union > 0 && float64(shared)/float64(union) >= 0.6
}

// observePreamble tracks near-duplicate assistant preambles within a turn.
// warn fires once, on the first repeat; stop fires when the model keeps
// echoing after being told to cut it out.
func (m *Model) observePreamble(preamble string) (warn, stop bool) {
	norm := normalizePreamble(preamble)
	if len(norm) < minPreambleLen {
		return false, false
	}
	if similarPreamble(norm, m.lastPreamble) {
		m.preambleStreak++
	} else {
		m.preambleStreak = 0
	}
	m.lastPreamble = norm
	if m.preambleStreak >= 1 && !m.preambleWarned {
		m.preambleWarned = true
		warn = true
	}
	return warn, m.preambleStreak >= 3
}

// canonicalJSON/callFingerprint/isOscillating moved to package tools
// (CanonicalJSON/CallFingerprint/IsOscillating), and salvageJSON/repairHint/
// shouldFormatRepair to tools too (SalvageJSON/RepairHint/ShouldFormatRepair), so
// the headless sub-agent loop reuses the same tool-call safety. See
// tools/loopguard.go and tools/repair.go.
