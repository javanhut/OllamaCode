package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// Loop-safety tunables.
const (
	defaultMaxSteps     = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures = 2  // identical failing call attempts before short-circuit
	recentOutcomesKept  = 12 // round outcome ring length for oscillation detection
	maxAutoContinues    = 3  // times we nudge the model to keep going on open todos before yielding
	maxStreamRetries    = 2  // transient stream errors auto-retried per turn before surfacing
	// autoModeMaxSteps replaces the per-turn round budget in auto mode, which is
	// meant to run unattended to completion. Every reader of the budget must use
	// this same number: the step gate, the text-form tool-call fallback and the
	// turn-end todo reconcile each ask "did this turn run out of budget?", and two
	// literals drifting apart silently changes whether stale todos get closed.
	autoModeMaxSteps = 100
)

// Stream retry backoff tunables.
const (
	streamRetryBaseDelay   = 1 * time.Second  // first retry; doubles per attempt
	streamRetryMaxDelay    = 30 * time.Second // cap on our own backoff
	streamRetryProviderMax = 5 * time.Minute  // cap even on a provider's Retry-After ask
)

// streamRetryDelay computes the wait before retry attempt n (1-based):
// exponential backoff with ±25% jitter, capped. A provider Retry-After hint
// overrides our schedule entirely — the provider knows its own queue — but is
// still capped so a hostile or broken header cannot park a turn for hours.
func streamRetryDelay(attempt int, err error) time.Duration {
	if d, ok := api.RetryAfterDelay(err); ok {
		return min(d, streamRetryProviderMax)
	}
	delay := streamRetryBaseDelay << (attempt - 1)
	if delay > streamRetryMaxDelay {
		delay = streamRetryMaxDelay
	}
	jitter := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(delay) * jitter)
}

// streamRetryable reports whether a stream error is worth burning a retry on.
// Deterministic refusals are excluded: a context overflow has its own
// compaction path, an OOM was already retried down to the smallest context
// that could load, and a format rejection its downgrade ladder — an unchanged
// resend of any of them fails identically. Transient-looking bodies and
// statuses retry even on an ambiguous status, so the transient check runs
// before IsFormatRejection, which reads ANY 400; explicit 4xx refusals without
// transient text do not heal on resend. Anything else stays retryable,
// preserving the historical behavior of giving an unknown failure the benefit
// of the (bounded) budget.
func streamRetryable(err error) bool {
	if agent.IsContextOverflow(err) || api.IsMemoryFailure(err) {
		return false
	}
	if api.IsTransientError(err) {
		return true
	}
	if agent.IsFormatRejection(err) {
		return false
	}
	switch api.StatusCodeOf(err) {
	case 400, 401, 403, 404:
		return false
	}
	return true
}

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
	// Overflow recovery is scoped to the USER TURN, not the session — without
	// this reset a session gets exactly one forced compaction ever. It is also
	// what makes esc safe: interruptTurn comes through here, so a compactDoneMsg
	// landing after an abandoned turn finds no retry owed.
	m.overflowErr = nil
	m.overflowTokens = 0
	m.overflowRetried = false
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
	m.turnStoppedByGuard = false
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
	m.loopEscalation = nil
	m.loopGuardCursor = 0
	if m.loopContinues == nil {
		m.loopContinues = map[string]int{}
	} else {
		clear(m.loopContinues)
	}
}

// advisory builds a loop-guard message addressed to the model. It rides the
// "user" role rather than "system" because many open-weight chat templates
// (Qwen, Llama and Mistral variants) only honor a LEADING system message and
// reorder, merge, or silently drop one that shows up mid-conversation — and the
// message that breaks a loop is the one that must not be dropped. Callers append
// it after the batch's tool results, so it reads as an ordinary turn instead of
// splicing into a call/result pair the way a system message did.
func advisory(content string) api.Message {
	return api.Message{Role: "user", Content: content, Advisory: true}
}

// isUserTurn reports whether a message is something the human actually typed,
// as opposed to an advisory riding the same role. Every backwards scan looking
// for "the last thing the user asked for" wants this: checkpoint labels, the
// turn anchor, the citation gate's once-per-turn latch. Role == "user" alone
// now stops on the guards' own output.
func isUserTurn(msg api.Message) bool {
	return msg.Role == "user" && !msg.Advisory
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

// observeStagnation counts consecutive rounds that produced no evidence this
// turn had not already seen, whatever tools produced them. Batch shape is not
// the signal: one tool repeated with cosmetically different arguments defeats
// the repeat guard exactly as easily as a rotating mixed batch does — a logged
// session spent 33 no-op todo_write rounds proving it. Thresholds mirror the
// 3/5 policy: the first round is evidence, two repeated no-evidence rounds warn
// (round 3), and four end the turn (round 5) — rather than discovering the loop
// by exhausting a 40-round step budget.
func (m *Model) observeStagnation(progress bool) (warn, stop bool) {
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

// stopForBlockerReport disables tools for the model's next message because a
// guard stopped this turn — a loop, no progress, the step budget, a check that
// will not go green. The reply that follows is a blocker report.
//
// Separate from the bare suppressToolsOnce the citation gate sets: that one
// withholds tools to re-ask for the SAME answer with citations, which is still
// an ordinary answer. The distinction is what markPlanPresented reads.
func (m *Model) stopForBlockerReport() {
	m.suppressToolsOnce = true
	m.turnStoppedByGuard = true
}

// Doom-loop escalation: when a detector crosses its stop threshold the human
// gets the call — stop the turn, let the agent keep going, or take the looping
// tool away — instead of the turn ending on the spot. Ported from opencode's
// doom_loop permission ask.

// maxLoopContinues caps how many times per user turn the same detector's stop
// can be waved through with "continue anyway". Past the cap the automatic stop
// fires, or a looping model could farm the modal forever.
const maxLoopContinues = 2

// Loop detector names, used as keys into m.loopContinues.
const (
	loopStopRepeat      = "repeat"
	loopStopOscillation = "oscillation"
	loopStopStagnation  = "stagnation"
)

// loopEscalation is a detector stop parked while the user decides. It carries
// every flag the automatic path would have acted on, so "stop turn" can
// reproduce that path exactly (applyLoopGuardStops) even when several
// detectors crossed in the same round.
type loopEscalation struct {
	kind            string // loopStop* — the detector the modal is about
	tool            string // repeat only: the streaking tool
	streak          int    // sameToolStreak at the crossing, for the reason text
	rounds          int    // stagnantRounds at the crossing, for the reason text
	stopOscillation bool
	stopStagnant    bool
	stopRepeat      bool
	announceStop    bool
}

// reason is the one-line human explanation shown in the modal.
func (esc *loopEscalation) reason() string {
	switch esc.kind {
	case loopStopRepeat:
		return fmt.Sprintf("repeated identical %s calls %d times", esc.tool, esc.streak)
	case loopStopOscillation:
		return "oscillating between the same two actions and results"
	default:
		return fmt.Sprintf("no progress for %d rounds", esc.rounds)
	}
}

// loopStopKind picks which detector the escalation is about when several cross
// in the same round. Repeat speaks first: "repeated identical X calls" names
// the culprit and is the only stop that can offer the ban choice. Oscillation
// outranks stagnation because "oscillating between A and B" is the more
// specific diagnosis of the same wasted rounds. Only the modal is about one
// detector — the stop path itself still applies every flag that fired.
func loopStopKind(stopOscillation, stopStagnant, stopRepeat bool, batchTool string) (kind, tool string) {
	switch {
	case stopRepeat:
		return loopStopRepeat, batchTool
	case stopOscillation:
		return loopStopOscillation, ""
	case stopStagnant:
		return loopStopStagnation, ""
	}
	return "", ""
}

// shouldAskLoopEscalation reports whether a detector stop becomes a user
// prompt instead of the automatic turn ending. Two cases keep the old
// behavior: auto mode runs unattended, so a modal would park the turn forever;
// and a detector the user already waved through twice this turn stops on its
// own rather than spamming a third identical prompt.
func (m *Model) shouldAskLoopEscalation(kind string) bool {
	if m.mode == AutoMode {
		return false
	}
	return m.loopContinues[kind] < maxLoopContinues
}

// resetLoopDetector clears one detector's streak after the user chose to keep
// the turn going. The other guards stay armed, and the warn latches stay
// latched — the modal, not another advisory, is now this detector's
// escalation channel.
func (m *Model) resetLoopDetector(kind string) {
	switch kind {
	case loopStopRepeat:
		m.sameToolStreak = 0
		m.lastStepRepeatKey = ""
		// Clear the second-crossing ban latch too: the next streak earns a
		// fresh escalation instead of an automatic ban the user never chose.
		m.stopWarnedTool = ""
	case loopStopOscillation:
		m.oscillationStreak = 0
	case loopStopStagnation:
		m.stagnantRounds = 0
	}
}

// loopBanAllowed mirrors the automatic ban's exclusions in
// observeRepeatedBatch: switch_mode is the only route out of a read-only mode,
// and an argument-keyed tool's streak was earned by one target, so banning the
// name would take away every other search.
func loopBanAllowed(tool string) bool {
	return tool != "" && tool != "switch_mode" && !argumentSensitiveRepeatTools[tool]
}

// applyLoopGuardStops is the automatic stop the guards always performed:
// advisories to the model plus a tool-less blocker-report reply. It runs when
// the user picks "stop turn", when the modal is capped or unavailable, and
// when another guard already ended the turn while an escalation was parked.
func (m *Model) applyLoopGuardStops(esc *loopEscalation) {
	if esc.stopOscillation {
		m.history = append(m.history, advisory("[LOOP BROKEN] The same A/B outcomes continued after the warning. Tools are disabled for your next message — explain the blocker and summarize what you know."))
		m.stopForBlockerReport()
	}
	if esc.stopStagnant {
		m.history = append(m.history, advisory("[TURN ENDED — NO PROGRESS] Five rounds of tool calls produced no new information, so this turn is over. Reply once, in plain text: what you found, what you changed, and what is left. Tools are disabled for that reply; only a failed verification of code you changed can bring you back this turn. If you were waiting for something to finish, poll it with run_shell(background=true) plus shell_output instead of repeating the same command."))
		m.stopForBlockerReport()
		// A tool-less message is not an ending on its own: the auto-continue on
		// open todos and the citation gate each pull the model straight back in,
		// which is exactly what kept the logged loop fed. This flag closes those
		// doors so the reply reaches endTurnTail.
		m.endTurnAfterReply = true
	}
	if esc.announceStop && !esc.stopStagnant {
		content := fmt.Sprintf("[LOOP BROKEN] You called %q %d times in a row. Tools are disabled for your next message — respond to the user in plain text only.", esc.tool, esc.streak)
		if m.bannedTools[esc.tool] {
			content = fmt.Sprintf("[TOOL DISABLED] You called %q %d times in a row without making progress, so it is removed from your tools for the rest of this turn — calling it in text will be refused too. Finish with the tools you still have, or answer the user in plain text.", esc.tool, esc.streak)
		}
		m.history = append(m.history, advisory(content))
	}
	if esc.stopRepeat {
		m.stopForBlockerReport()
	}
}
