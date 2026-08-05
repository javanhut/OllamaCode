package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// Loop-safety tunables.
const (
	defaultMaxSteps     = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures = 2  // identical failing call attempts before short-circuit
	recentOutcomesKept  = 12 // round outcome ring length for oscillation detection
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
	m.recentOutcomes = m.recentOutcomes[:0]
	m.oscillationStreak = 0
	m.stagnantRounds = 0
	if m.seenOutcomes == nil {
		m.seenOutcomes = map[string]bool{}
	} else {
		clear(m.seenOutcomes)
	}
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
// a single-tool batch. Mixed batches return empty identities and are handled by
// the round-outcome stagnation guard instead.
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
func (m *Model) observeRepeatedBatch(calls []tools.ToolCall, madeProgress ...bool) (tool string, warn, stop, announceStop bool) {
	tool, key := batchRepeatIdentity(calls)
	if key == "" {
		m.lastStepRepeatKey = ""
		m.sameToolStreak = 0
		return "", false, false, false
	}
	// A repeated tool name is not itself stagnation. A new mutation, diagnostic,
	// target, or result is material evidence and restarts the tolerance window.
	if len(madeProgress) > 0 && madeProgress[0] {
		m.lastStepRepeatKey = key
		m.sameToolStreak = 1
		return tool, false, false, false
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

// roundOutcomeIdentity summarizes what a tool round actually observed. Calls
// and results are paired and hashed so large file reads are not retained twice.
// Arguments matter: editing a second file or searching for a new symbol is new
// evidence, while the same call returning the same result is not progress.
func roundOutcomeIdentity(calls []tools.ToolCall, results []api.Message) string {
	parts := make([]string, 0, len(calls))
	for i, call := range calls {
		result := ""
		if i < len(results) {
			result = strings.TrimSpace(results[i].Content)
		}
		callID := tools.CallFingerprint(call)
		// switch_mode's free-form reason can vary without changing the action.
		// Its result carries the observable state transition (or lack of one).
		if call.Function.Name == "switch_mode" {
			callID = call.Function.Name
		}
		sum := sha256.Sum256([]byte(callID + "\x00" + result))
		parts = append(parts, fmt.Sprintf("%x", sum))
	}
	sort.Strings(parts) // reordered parallel calls describe the same outcome
	return strings.Join(parts, "\x01")
}

// observeRoundProgress distinguishes activity from forward motion. A round is
// progress when it produces evidence not already seen this turn. Stable A/B/A/B
// outcomes warn immediately and hard-stop if they continue for two more rounds.
func (m *Model) observeRoundProgress(calls []tools.ToolCall, results []api.Message) (progress, warnOscillation, stopOscillation bool) {
	if m.seenOutcomes == nil {
		m.seenOutcomes = map[string]bool{}
	}
	outcome := roundOutcomeIdentity(calls, results)
	progress = outcome != "" && !m.seenOutcomes[outcome]
	// A locally refused duplicate is enforcement feedback, not new knowledge,
	// even though its text differs from the original tool failure.
	if len(results) > 0 {
		allRefusedRepeats := true
		for _, result := range results {
			if !strings.Contains(result.Content, "you already called") && !strings.Contains(result.Content, "you already ran") {
				allRefusedRepeats = false
				break
			}
		}
		if allRefusedRepeats {
			progress = false
		}
	}
	if outcome != "" {
		m.seenOutcomes[outcome] = true
		m.recentOutcomes = append(m.recentOutcomes, outcome)
		if len(m.recentOutcomes) > recentOutcomesKept {
			m.recentOutcomes = m.recentOutcomes[len(m.recentOutcomes)-recentOutcomesKept:]
		}
	}
	if tools.IsOscillating(m.recentOutcomes) {
		m.oscillationStreak++
		warnOscillation = m.oscillationStreak == 1
		stopOscillation = m.oscillationStreak >= 3
	} else {
		m.oscillationStreak = 0
	}
	return progress, warnOscillation, stopOscillation
}

// observeMixedBatchStagnation covers repeated mixed-tool batches, for which a
// single tool name cannot describe the loop. Thresholds mirror the 3/5 policy:
// the first round is evidence, two repeated no-evidence rounds warn (round 3),
// and four stop (round 5).
func (m *Model) observeMixedBatchStagnation(progress bool) (warn, stop bool) {
	if progress {
		m.stagnantRounds = 0
		return false, false
	}
	m.stagnantRounds++
	return m.stagnantRounds == 2, m.stagnantRounds >= 4
}

// maxReadsPerUnchangedTarget allows one initial read and one warned repeat. A
// third read of that same unchanged target suppresses tools for the next reply.
const maxReadsPerUnchangedTarget = 3

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
		if m.turnReads[key] >= maxReadsPerUnchangedTarget {
			stop = true
		}
	}
	return rereads, stop
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
