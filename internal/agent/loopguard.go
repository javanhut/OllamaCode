package agent

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

// Loop detector names. The interactive UI keys its "continue anyway" grants
// and escalation modal on these.
const (
	LoopStopRepeat      = "repeat"
	LoopStopOscillation = "oscillation"
	LoopStopStagnation  = "stagnation"
)

// recentOutcomesKept is the round-outcome ring length for oscillation detection.
const recentOutcomesKept = 12

// MaxReadsPerUnchangedTarget allows one initial read and one warned repeat. A
// third read of that same unchanged target suppresses tools for the next reply.
const MaxReadsPerUnchangedTarget = 3

// minPreambleLen skips trivially short preambles ("OK", "Sure") — two of
// those in a row is a style choice, not an echo loop.
const minPreambleLen = 24

// LoopGuard holds the per-turn detectors that stop a confused model from
// spinning: repeated single-tool batches, A/B outcome oscillation, rounds that
// produce no new evidence, re-reads of unchanged files, and echoed preambles.
// It only observes and reports; what to say to the model and whether to end
// the turn is the caller's decision. The zero value is ready to use; call
// Reset at the start of every user turn.
type LoopGuard struct {
	// Read by callers to word advisories and escalations.
	SameToolStreak      int             // consecutive steps repeating the same batch identity
	StagnantRounds      int             // consecutive rounds that produced no evidence this turn had not already seen
	OscillationWarned   bool            // caller latch: corrective oscillation nudge emitted this turn
	RereadStopAnnounced bool            // caller latch: re-read hard-stop explanation emitted this turn
	BannedTools         map[string]bool // tools withdrawn for the rest of this turn
	TurnReads           map[string]int  // read tool + "\x01" + cleaned path -> times read this turn

	lastRepeatKey     string          // semantic identity of the previous single-tool batch
	sameToolWarned    bool            // early repeat warning emitted this turn
	stopWarnedTool    string          // tool the hard stop already fired for this turn
	recentOutcomes    []string        // round-level call+result identities
	seenOutcomes      map[string]bool // evidence already observed this turn
	oscillationStreak int             // consecutive round endings that still form A/B alternation
	lastPreamble      string          // normalized previous assistant preamble
	preambleStreak    int             // consecutive near-duplicate preambles
	preambleWarned    bool            // preamble-echo warning emitted this turn
}

// Reset clears all per-turn state, keeping allocated maps.
func (g *LoopGuard) Reset() {
	*g = LoopGuard{
		BannedTools:    g.BannedTools,
		TurnReads:      g.TurnReads,
		seenOutcomes:   g.seenOutcomes,
		recentOutcomes: g.recentOutcomes[:0],
	}
	if g.BannedTools == nil {
		g.BannedTools = map[string]bool{}
	}
	clear(g.BannedTools)
	clear(g.TurnReads)
	clear(g.seenOutcomes)
}

// Ban withdraws a tool for the rest of the turn.
func (g *LoopGuard) Ban(tool string) {
	if g.BannedTools == nil {
		g.BannedTools = map[string]bool{}
	}
	g.BannedTools[tool] = true
}

// ResetDetector clears one detector's streak after the user chose to keep the
// turn going. The other detectors stay armed and the warn latches stay latched.
func (g *LoopGuard) ResetDetector(kind string) {
	switch kind {
	case LoopStopRepeat:
		g.SameToolStreak = 0
		g.lastRepeatKey = ""
		// Clear the second-crossing ban latch too: the next streak earns a
		// fresh escalation instead of an automatic ban the user never chose.
		g.stopWarnedTool = ""
	case LoopStopOscillation:
		g.oscillationStreak = 0
	case LoopStopStagnation:
		g.StagnantRounds = 0
	}
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

// LoopBanAllowed reports whether a repeating tool may be withdrawn. switch_mode
// is the only route out of a read-only mode, and an argument-keyed tool's
// streak was earned by one target, so banning the name would take away every
// other search and make "try something different" impossible to follow. Those
// repeats are covered per-target by the failed-call short-circuit and the
// re-read guard.
func LoopBanAllowed(tool string) bool {
	return tool != "" && tool != "switch_mode" && !argumentSensitiveRepeatTools[tool]
}

// BatchSingleTool returns the tool name if every call in a batch is the same
// tool, else "". Used to detect a model spamming one tool (e.g. switch_mode)
// with varying arguments — which evades fingerprint-based repeat detection.
func BatchSingleTool(calls []tools.ToolCall) string {
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

// batchRepeatIdentity returns the display tool name and semantic repeat key for
// a single-tool batch. Mixed batches return empty identities and are handled by
// the round-outcome stagnation guard instead.
func batchRepeatIdentity(calls []tools.ToolCall) (string, string) {
	tool := BatchSingleTool(calls)
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

// ObserveRepeatedBatch advances the per-turn repetition state. warn is emitted
// at most once per turn. stop remains true after the hard threshold so a
// text-form tool call cannot bypass a single tool-less response; announceStop
// re-fires on every round past the threshold, because latching it once let a
// looping model run 22 further rounds with no feedback at all. Crossing the
// threshold a second time for the same tool bans it for the rest of the turn.
func (g *LoopGuard) ObserveRepeatedBatch(calls []tools.ToolCall, madeProgress ...bool) (tool string, warn, stop, announceStop bool) {
	tool, key := batchRepeatIdentity(calls)
	if key == "" {
		g.lastRepeatKey = ""
		g.SameToolStreak = 0
		return "", false, false, false
	}
	// A repeated tool name is not itself stagnation. A new mutation, diagnostic,
	// target, or result is material evidence and restarts the tolerance window —
	// but a bookkeeping tool cannot certify its own progress, or cosmetic churn
	// in its arguments resets the window for free.
	if len(madeProgress) > 0 && madeProgress[0] && !bookkeepingTools[tool] {
		g.lastRepeatKey = key
		g.SameToolStreak = 1
		return tool, false, false, false
	}
	if key == g.lastRepeatKey {
		g.SameToolStreak++
	} else {
		g.lastRepeatKey = key
		g.SameToolStreak = 1
	}
	if g.SameToolStreak >= 3 && !g.sameToolWarned {
		g.sameToolWarned = true
		warn = true
	}
	if g.SameToolStreak >= 5 {
		stop = true
		announceStop = true
		// Muting every tool for one message has already been tried for this tool
		// and it kept calling it, so take the tool itself away instead: the model
		// keeps working with everything else rather than losing the whole turn.
		if g.stopWarnedTool == tool && LoopBanAllowed(tool) {
			g.Ban(tool)
		}
		g.stopWarnedTool = tool
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

// ObserveRoundProgress distinguishes activity from forward motion. A round is
// progress when it produces evidence not already seen this turn. Stable A/B/A/B
// outcomes warn immediately and hard-stop if they continue for two more rounds.
func (g *LoopGuard) ObserveRoundProgress(calls []tools.ToolCall, results []api.Message) (progress, warnOscillation, stopOscillation bool) {
	if g.seenOutcomes == nil {
		g.seenOutcomes = map[string]bool{}
	}
	outcome := roundOutcomeIdentity(calls, results)
	progress = outcome != "" && !g.seenOutcomes[outcome]
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
	if outcome != "" {
		g.seenOutcomes[outcome] = true
		g.recentOutcomes = append(g.recentOutcomes, outcome)
		if len(g.recentOutcomes) > recentOutcomesKept {
			g.recentOutcomes = g.recentOutcomes[len(g.recentOutcomes)-recentOutcomesKept:]
		}
	}
	if tools.IsOscillating(g.recentOutcomes) {
		g.oscillationStreak++
		warnOscillation = g.oscillationStreak == 1
		stopOscillation = g.oscillationStreak >= 3
	} else {
		g.oscillationStreak = 0
	}
	return progress, warnOscillation, stopOscillation
}

// ObserveStagnation counts consecutive rounds that produced no evidence this
// turn had not already seen, whatever tools produced them. Batch shape is not
// the signal: one tool repeated with cosmetically different arguments defeats
// the repeat guard exactly as easily as a rotating mixed batch does — a logged
// session spent 33 no-op todo_write rounds proving it. Thresholds mirror the
// 3/5 policy: the first round is evidence, two repeated no-evidence rounds warn
// (round 3), and four end the turn (round 5) — rather than discovering the loop
// by exhausting a 40-round step budget.
func (g *LoopGuard) ObserveStagnation(progress bool) (warn, stop bool) {
	if progress {
		g.StagnantRounds = 0
		return false, false
	}
	g.StagnantRounds++
	return g.StagnantRounds == 2, g.StagnantRounds >= 4
}

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

// ObserveFileReads records path-keyed reads for the turn and reports targets
// the model has already read without an intervening mutation. The streak
// guard misses these because it resets whenever any other call interleaves.
func (g *LoopGuard) ObserveFileReads(calls []tools.ToolCall) (rereads []string, stop bool) {
	if g.TurnReads == nil {
		g.TurnReads = map[string]int{}
	}
	seen := map[string]bool{}
	for _, call := range calls {
		key, ok := readTargetKey(call)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		g.TurnReads[key]++
		if g.TurnReads[key] > 1 {
			rereads = append(rereads, strings.SplitN(key, "\x01", 2)[1])
		}
		if g.TurnReads[key] >= MaxReadsPerUnchangedTarget {
			stop = true
		}
	}
	return rereads, stop
}

// ForgetReads drops read records for paths a mutating tool just changed, so
// re-reading a file after editing it is never treated as a loop.
func (g *LoopGuard) ForgetReads(paths []string) {
	for _, p := range paths {
		clean := filepath.Clean(p)
		for tool := range pathKeyedReadTools {
			delete(g.TurnReads, tool+"\x01"+clean)
		}
	}
}

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

// ObservePreamble tracks near-duplicate assistant preambles within a turn.
// warn fires once, on the first repeat; stop fires when the model keeps
// echoing after being told to cut it out.
func (g *LoopGuard) ObservePreamble(preamble string) (warn, stop bool) {
	norm := normalizePreamble(preamble)
	if len(norm) < minPreambleLen {
		return false, false
	}
	if similarPreamble(norm, g.lastPreamble) {
		g.preambleStreak++
	} else {
		g.preambleStreak = 0
	}
	g.lastPreamble = norm
	if g.preambleStreak >= 1 && !g.preambleWarned {
		g.preambleWarned = true
		warn = true
	}
	return warn, g.preambleStreak >= 3
}

// Advisory texts the guards' warnings put in front of the model. Shared by the
// interactive loop and Run so both teach the model the same way.
const (
	AdvisoryOscillation = "[NO PROGRESS DETECTED] You are alternating between the same actions and receiving the same results. Stop, state your blocker explicitly, and try a materially different approach."
	AdvisoryStagnation  = "[NO PROGRESS DETECTED] Your last three rounds of tool calls returned nothing this turn has not already seen. Use the evidence you have, take a materially different action, or state the blocker."
)

// AdvisoryRepeat warns a model that keeps calling one tool without progress.
func AdvisoryRepeat(tool string, streak int) string {
	return fmt.Sprintf("[REPEATING ACTION] You have called %q %d times in a row without making progress. Stop repeating it — take a different action, or if you're blocked, explain the blocker to the user in plain text.", tool, streak)
}

// AdvisoryReread tells a model it already holds the contents it is re-reading.
func AdvisoryReread(paths []string) string {
	return fmt.Sprintf("[RE-READ DETECTED] You already read \"%s\" this turn and nothing has changed it since — you have the contents. Use them, or grep for the specific thing you need instead of re-reading whole files.", strings.Join(paths, `", "`))
}

// BannedToolRefusal is the tool result for a call to a tool the repeat guard
// withdrew. ObserveRoundProgress recognizes it as enforcement, not evidence.
func BannedToolRefusal(tool string) string {
	return fmt.Sprintf("error: %q is disabled for the rest of this turn — you called it repeatedly without making progress. Do not call it again; act on what you already know or answer the user.", tool)
}
