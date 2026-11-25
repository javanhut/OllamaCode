package tui

import (
	"errors"
	"fmt"
	"math/rand"
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
	delay := min(streamRetryBaseDelay<<(attempt-1), streamRetryMaxDelay)
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

// errRunawayModelStream ends a stream that is still emitting tokens but has
// stopped saying anything new. It rides the normal stream-error path so the
// turn gets the usual one retry rather than a special-cased recovery.
var errRunawayModelStream = errors.New("model output loop detected — response stopped making progress")

// streamOutputRunaway catches a model that is still producing tokens but has
// stopped making progress. Idle timeouts cannot see this failure mode: the
// connection is healthy, the transcript is just filling with the same bytes.
func streamOutputRunaway(content string, constrained bool) bool {
	limit := 64 * 1024
	if constrained {
		// A constrained reply is one tool call or one short prose envelope.
		// Anything near this size is already a loop.
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
	// The search is limited to the recent tail: a real loop repeats NOW, while a
	// long legitimate answer may well contain the same boilerplate line four
	// times over 60KB and must not be killed for it.
	// ponytail: O(window) substring scans per chunk at 30 Hz; index the tail if
	// the window ever grows.
	const window = 8 * 1024
	for offset := 0; offset <= 48; offset += 16 {
		end := len(content) - offset
		if end < 64 {
			continue
		}
		fragment := content[end-64 : end]
		if strings.Count(content[max(0, end-64-window):end-64], fragment) >= 4 {
			return true
		}
	}
	return false
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
	// The next stream this turn starts will be turnGen+1, and /rate has to
	// cover every round the turn goes on to produce. The rateable span is NOT
	// cleared here: endTurnTail dequeues a queued message, so this runs inside
	// the completion of the very turn that just became rateable.
	m.turnFirstGen = m.turnGen + 1
	m.stepCount = 0
	m.streamRetries = 0
	// Overflow recovery is scoped to the USER TURN, not the session — without
	// this reset a session gets exactly one forced compaction ever. It is also
	// what makes esc safe: interruptTurn comes through here, so a compactDoneMsg
	// landing after an abandoned turn finds no retry owed.
	m.overflowErr = nil
	m.overflowTokens = 0
	m.overflowRetried = false
	m.guard.Reset()
	m.suppressToolsOnce = false
	m.endTurnAfterReply = false
	m.turnStoppedByGuard = false
	m.turnTouchedFiles = false
	m.fetchedContent = false
	if m.turnChangedPaths == nil {
		m.turnChangedPaths = map[string]bool{}
	} else {
		clear(m.turnChangedPaths)
	}
	m.lastVerification = ""
	m.verifyAttempts = 0
	m.verifyTracker, m.integrityCheckedThisTurn, m.resampling = agent.VerifyTracker{Max: maxVerifyAttempts}, false, false
	m.challengedThisTurn = false
	m.reviewedThisTurn = false
	m.autoContinues = 0
	for k := range m.failedCalls {
		delete(m.failedCalls, k)
	}
	m.planNeedsVerify = false
	m.planPaths = nil
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

// loopEscalation is a detector stop parked while the user decides. It carries
// every flag the automatic path would have acted on, so "stop turn" can
// reproduce that path exactly (applyLoopGuardStops) even when several
// detectors crossed in the same round.
type loopEscalation struct {
	kind            string // agent.LoopStop* — the detector the modal is about
	tool            string // repeat only: the streaking tool
	streak          int    // guard.SameToolStreak at the crossing, for the reason text
	rounds          int    // guard.StagnantRounds at the crossing, for the reason text
	stopOscillation bool
	stopStagnant    bool
	stopRepeat      bool
	announceStop    bool
}

// reason is the one-line human explanation shown in the modal.
func (esc *loopEscalation) reason() string {
	switch esc.kind {
	case agent.LoopStopRepeat:
		return fmt.Sprintf("repeated identical %s calls %d times", esc.tool, esc.streak)
	case agent.LoopStopOscillation:
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
		return agent.LoopStopRepeat, batchTool
	case stopOscillation:
		return agent.LoopStopOscillation, ""
	case stopStagnant:
		return agent.LoopStopStagnation, ""
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
		if m.guard.BannedTools[esc.tool] {
			content = fmt.Sprintf("[TOOL DISABLED] You called %q %d times in a row without making progress, so it is removed from your tools for the rest of this turn — calling it in text will be refused too. Finish with the tools you still have, or answer the user in plain text.", esc.tool, esc.streak)
		}
		m.history = append(m.history, advisory(content))
	}
	if esc.stopRepeat {
		m.stopForBlockerReport()
	}
}
