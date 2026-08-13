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
	defaultMaxSteps        = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures    = 2  // identical failing call attempts before short-circuit
	recentOutcomesKept     = 12 // round outcome ring length for oscillation detection
	maxAutoContinues       = 3  // times we nudge the model to keep going on open todos before yielding
	maxStreamRetries       = 2  // transient stream errors auto-retried per turn before surfacing
	maxDeadResponseRetries = 2  // empty completions truncated at the num_predict cap retried per turn
	maxActionDeferrals     = 1  // one corrective retry when prose promises a tool action but calls nothing
)

func maxStepsFromConfig(c config) int {
	if c.MaxSteps > 0 {
		return c.MaxSteps
	}
	return defaultMaxSteps
}

func (m *Model) turnStepLimit() int {
	if m.profile.ProfileMaxSteps > 0 {
		return m.profile.ProfileMaxSteps
	}
	return m.maxSteps
}

// resetTurnGuards clears the per-turn loop-safety state. Call at the start of
// every new user turn (fresh submit or a dequeued message).
func (m *Model) resetTurnGuards() {
	m.startTurnClock()
	m.stepCount = 0
	m.streamRetries = 0
	m.degradedStreamRetry = false
	m.deadResponseRetries = 0
	m.numPredictOverride = 0
	m.lastTodoOpenCount = m.todos.openCount()
	m.actionDeferrals = 0
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
	m.endTurnAfterReply = false
	m.lastStepRepeatKey = ""
	m.sameToolStreak = 0
	m.sameToolWarned = false
	m.stopWarnedTool = ""
	if m.bannedTools == nil {
		m.bannedTools = map[string]bool{}
	} else {
		clear(m.bannedTools)
	}
	m.turnTouchedFiles = false
	m.fetchedContent = false
	if m.turnChangedPaths == nil {
		m.turnChangedPaths = map[string]bool{}
	} else {
		clear(m.turnChangedPaths)
	}
	m.lastVerification = ""
	m.verifyAttempts = 0
	m.challengedThisTurn = false
	m.reviewedThisTurn = false
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

// streamOutputRunaway catches a model that is still producing tokens but has
// stopped making progress. Idle timeouts cannot see this failure mode.
func streamOutputRunaway(content string, constrained bool) bool {
	limit := 64 * 1024
	if constrained {
		limit = 8 * 1024
	}
	if len(content) > limit {
		return true
	}
	if len(content) < 384 {
		return false
	}
	for _, marker := range []string{`\"mode\":\"write\"`, `"mode":"write"`, `\"name\":\"switch_mode\"`, `"name":"switch_mode"`} {
		if strings.Count(content, marker) >= 4 {
			return true
		}
	}
	// Sample several suffix positions so chunk boundaries do not hide a repeated
	// phrase. Four prior copies of a 64-byte fragment is strong loop evidence.
	for offset := 0; offset <= 48; offset += 16 {
		end := len(content) - offset
		if end < 64 {
			continue
		}
		fragment := content[end-64 : end]
		if strings.Count(content[:end-64], fragment) >= 4 {
			return true
		}
	}
	return false
}

func promisesToolAction(content string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(content), " "))
	for _, phrase := range []string{
		"let me check", "let me inspect", "let me look", "let me first check",
		"i'll check", "i will check", "i'll inspect", "i will inspect",
		"let me start by exploring", "let me start by checking",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
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

// argumentSensitiveRepeatTools are naturally iterative operations whose
// arguments carry the whole action. Different arguments mean the model is
// gathering different information (or recording different findings), not
// repeating itself. Exact repeats are still guarded. The notes writers belong
// here because their argument IS the content: writing a long plan in five
// sections is five distinct actions, and treating it as a streak suppressed
// tools mid-plan and could ban the tool plan mode requires.
var argumentSensitiveRepeatTools = map[string]bool{
	"read_file": true, "list_directory": true, "find_files": true,
	"grep": true, "file_info": true, "find_symbol": true,
	"code_definition": true, "code_references": true, "code_hover": true,
	"semantic_search": true, "git_diff": true, "git_log": true,
	"update_session_notes": true, "append_session_notes": true,
}

// bookkeepingTools mutate nothing in the workspace and answer no question about
// the code, so their own output can never be evidence of forward motion. A model
// that reworded one todo item ("Review ticket.md" -> "Read ticket.md") produced a
// fresh outcome hash every round, which reset the repeat tolerance window forever
// and bought it 33 todo_write calls in one turn.
var bookkeepingTools = map[string]bool{
	"todo_write": true, "todo_read": true, "switch_mode": true,
	"read_session_notes": true, "update_session_notes": true, "append_session_notes": true,
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
// re-fires on every round past the threshold, because latching it once let a
// looping model run 22 further rounds with no feedback at all. Crossing the
// threshold a second time for the same tool bans it for the rest of the turn
// (see m.bannedTools, applied in toolsForMode and enforced in dispatch).
func (m *Model) observeRepeatedBatch(calls []tools.ToolCall, madeProgress ...bool) (tool string, warn, stop, announceStop bool) {
	tool, key := batchRepeatIdentity(calls)
	if key == "" {
		m.lastStepRepeatKey = ""
		m.sameToolStreak = 0
		return "", false, false, false
	}
	// A repeated tool name is not itself stagnation. A new mutation, diagnostic,
	// target, or result is material evidence and restarts the tolerance window —
	// but a bookkeeping tool cannot certify its own progress, or cosmetic churn
	// in its arguments resets the window for free.
	if len(madeProgress) > 0 && madeProgress[0] && !bookkeepingTools[tool] {
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
		announceStop = true
		// Muting every tool for one message has already been tried for this tool
		// and it kept calling it, so take the tool itself away instead: the model
		// keeps working with everything else rather than losing the whole turn.
		// Two kinds of tool are never withdrawn: switch_mode, the only route out
		// of a read-only mode, and anything argument-keyed above — its streak was
		// earned by one grep pattern or one file path, so banning the name would
		// take away every other search and make the "try something different"
		// instruction impossible to follow. Those repeats are already covered
		// per-target by the failed-call short-circuit and the re-read guard.
		if m.stopWarnedTool == tool && tool != "switch_mode" && !argumentSensitiveRepeatTools[tool] {
			m.bannedTools[tool] = true
		}
		m.stopWarnedTool = tool
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
	// A locally refused call is enforcement feedback, not new knowledge, even
	// though its text differs from the original tool failure. The ban refusal
	// counts too: it is identical for every argument, so a banned tool called
	// with reworded arguments would otherwise mint a fresh outcome hash and
	// reset the stagnation window on nothing but the harness's own answer.
	if len(results) > 0 {
		allRefusedRepeats := true
		for _, result := range results {
			if !strings.Contains(result.Content, "you already called") && !strings.Contains(result.Content, "you already ran") &&
				!strings.Contains(result.Content, "is disabled for the rest of this turn") {
				allRefusedRepeats = false
				break
			}
		}
		if allRefusedRepeats {
			progress = false
		}
	}
	// A round that changed observable state is forward motion even when its
	// outcome hash repeats earlier evidence: a long task legitimately performs
	// many similar edits, and an identical success text is not a loop.
	if !progress && m.roundMovedState(calls, results) {
		progress = true
	}
	m.lastTodoOpenCount = m.todos.openCount()
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

// roundMovedState reports whether the round changed observable state: a file
// mutation succeeded, or a todo_write moved the open-item count. Both are real
// progress the outcome hash cannot see — an edit retried after an intervening
// change returns the same success text, and todo results summarize identically.
func (m *Model) roundMovedState(calls []tools.ToolCall, results []api.Message) bool {
	for i, call := range calls {
		if i >= len(results) || !tools.ToolResultOK(results[i].Content) {
			continue
		}
		if len(tools.MutatedPaths(call.Function.Name, call.Function.Arguments)) > 0 {
			return true
		}
		if call.Function.Name == "todo_write" && m.todos.openCount() != m.lastTodoOpenCount {
			return true
		}
	}
	return false
}

// pollingOnlyRound reports whether every call in the round only observes state
// (reads, searches, status polls) and cannot mutate anything. A model polling a
// live background job gets the same "still running" text every round; combined
// with tools.BackgroundJobCount this keeps waiting from reading as looping.
func pollingOnlyRound(calls []tools.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		if tools.PolicyForName(call.Function.Name).Destructive {
			return false
		}
	}
	return true
}

// observeStagnation counts consecutive rounds that produced no evidence this
// turn had not already seen, whatever tools produced them. Batch shape is not
// the signal: one tool repeated with cosmetically different arguments defeats
// the repeat guard exactly as easily as a rotating mixed batch does — a logged
// session spent 33 no-op todo_write rounds proving it. Three consecutive
// no-evidence rounds warn and six end the turn, so a long task with legitimately
// repetitive reads (or a wait on a background job) gets room before the harness
// steps in — rather than discovering a real loop by exhausting the step budget.
func (m *Model) observeStagnation(progress bool) (warn, stop bool) {
	if progress {
		m.stagnantRounds = 0
		return false, false
	}
	m.stagnantRounds++
	return m.stagnantRounds == 3, m.stagnantRounds >= 6
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

// readObservation is the per-target re-read ledger entry: how often the target
// was read this turn and a hash of the last-seen result content. The hash is
// what separates a loop from a legitimate re-read — an edit made through
// run_shell (or any path the mutator ledger in forgetReads cannot see) changes
// the content, and re-reading a changed file is new evidence, not repetition.
type readObservation struct {
	count int
	hash  string // sha256 of the last read result for this target
}

// observeFileReads records path-keyed reads for the turn and reports targets
// the model has already read without the content changing since. The streak
// guard misses these because it resets whenever any other call interleaves.
func (m *Model) observeFileReads(calls []tools.ToolCall, results []api.Message) (rereads []string, stop bool) {
	if m.turnReads == nil {
		m.turnReads = map[string]readObservation{}
	}
	seen := map[string]bool{}
	for i, call := range calls {
		key, ok := readTargetKey(call)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		content := ""
		if i < len(results) {
			content = results[i].Content
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		obs := m.turnReads[key]
		if obs.count == 0 || obs.hash != hash {
			// First read, or the content changed since the last one: a
			// legitimate re-read. Reset the count and remember the new hash.
			m.turnReads[key] = readObservation{count: 1, hash: hash}
			continue
		}
		obs.count++
		m.turnReads[key] = obs
		if obs.count > 1 {
			rereads = append(rereads, strings.SplitN(key, "\x01", 2)[1])
			m.rereadEvents++
		}
		if obs.count >= maxReadsPerUnchangedTarget {
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
	for w := range strings.FieldsSeq(a) {
		setA[w] = true
	}
	shared, union := 0, len(setA)
	for w := range strings.FieldsSeq(b) {
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
