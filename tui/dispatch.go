package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/internal/safeshell"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

var diffPreviewTools = map[string]bool{
	"write_file": true, "edit_file": true, "append_file": true,
}

// webContentTools pull attacker-controlled bytes into the conversation. Their
// results are wrapped as UNTRUSTED, but the model still reasons over the
// text, so once any of these has been recorded this turn the fetched-content
// gate (noteFetchedContent / shouldPromptPermission) treats every later
// destructive call as potentially injected and asks the user to confirm it.
var webContentTools = map[string]bool{
	"web_fetch": true, "web_search": true, "web_search_api": true, "web_crawl": true,
}

type pendingBatch struct {
	calls      []tools.ToolCall
	results    []api.Message
	started    []bool
	done       int
	index      int
	allowAll   bool
	preview    string
	gen        int    // turn generation this batch belongs to
	deniedTool string // non-empty means the user ended this tool round
}

// pauseForUser closes the model-side activity state when a tool round ends at a
// human checkpoint. Native tool-call messages arrive before the stream state is
// cleared by chatDoneMsg, so without this the UI keeps showing THINKING and the
// user's answer is silently put in the queue with nothing left to drain it.
func (m *Model) pauseForUser(toast, reason string) {
	if m.stream != nil && m.stream.cancel != nil {
		m.stream.cancel()
	}
	if m.trace != nil {
		_ = m.trace.Record(tracepkg.Event{Kind: "turn_end", Turn: m.turnGen, Model: m.modelName,
			Metadata: map[string]any{"reason": reason, "steps": m.stepCount, "open_todos": m.todos.openCount()}})
	}
	m.streaming = false
	m.stream = nil
	m.busySince = time.Time{}
	m.toast = toast
	m.lastActivity = time.Now()
}

// freshnessLedger returns the session's stale-edit ledger, creating it on
// first use so test-constructed Models get the guard too. Callers on the
// update goroutine must pre-warm it before any tool goroutines start (see
// processPendingTools) so the lazy init never races.
func (m *Model) freshnessLedger() *tools.FreshnessLedger {
	if m.freshness == nil {
		m.freshness = tools.NewFreshnessLedger()
	}
	return m.freshness
}

func (m *Model) invokeTool(ctx context.Context, call tools.ToolCall) api.Message {
	m.logActivity("Tool: " + call.Function.Name)
	// Guard this call with the session's stale-edit ledger; the check itself
	// runs inside the registry (tools/freshness.go), so headless runs and
	// sub-agents get the same refusal from their own ledgers.
	ctx = tools.WithFreshnessLedger(ctx, m.freshnessLedger())
	executor := agent.Executor{
		Registry: m.tools, Host: m.host, Model: m.modelName, NumCtx: m.contextLimit,
		Before: m.checkpointBeforeCall(), Permissions: m.cfg.Permissions,
		Observe: func(event agent.ExecutionEvent) {
			if m.trace == nil {
				return
			}
			errText := ""
			if event.Err != nil {
				errText = event.Err.Error()
			}
			meta := map[string]any{"argument_failure": event.ArgumentFailure, "repair_attempted": event.RepairAttempted, "repair_succeeded": event.RepairSucceeded}
			if event.ExitCode != 0 {
				meta["exit_code"] = event.ExitCode
			}
			_ = m.trace.Record(tracepkg.Event{Kind: "tool", Turn: m.turnGen, Model: m.modelName,
				Tool: event.Call.Function.Name, Arguments: event.Call.Function.Arguments,
				Result: event.Result, Error: errText, DurationMS: event.Duration.Milliseconds(),
				Metadata: meta})
		},
	}
	event := executor.Execute(ctx, call)
	return api.Message{
		Role:     "tool",
		ToolName: call.Function.Name,
		Content:  event.Result,
	}
}

func (m *Model) recordPermission(call tools.ToolCall, decision string) {
	if m.trace == nil {
		return
	}
	_ = m.trace.Record(tracepkg.Event{Kind: "permission", Turn: m.turnGen, Model: m.modelName,
		Tool: call.Function.Name, Arguments: call.Function.Arguments, Metadata: map[string]any{"decision": decision}})
}

// savePermissionRule adds a rule to the live config and writes it to disk, so
// the decision survives the session. A rule already present is not duplicated.
func (m *Model) savePermissionRule(rule tools.PermissionRule) {
	for _, existing := range m.cfg.Permissions {
		if existing.Equal(rule) {
			m.toast = "rule already saved: " + rule.String()
			return
		}
	}
	m.cfg.Permissions = append(m.cfg.Permissions, rule)
	saveConfig(m.cfg)
	m.toast = "saved permission rule: " + rule.String()
}

func (m *Model) invokeToolCmd(gen, index int, call tools.ToolCall) tea.Cmd {
	return func() tea.Msg {
		var req *modeSwitchRequest
		if call.Function.Name == "switch_mode" {
			req, _ = parseModeSwitchArgs(call.Function.Arguments)
		}

		timeout := tools.ToolCallTimeout(call)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		done := make(chan api.Message, 1)
		go func() {
			// A panic in a tool handler must not tear down the whole program;
			// convert it into an error result so the turn can recover.
			defer func() {
				if r := recover(); r != nil {
					done <- api.Message{
						Role:     "tool",
						ToolName: call.Function.Name,
						Content:  fmt.Sprintf("error: tool %q panicked: %v", call.Function.Name, r),
					}
				}
			}()
			done <- m.invokeTool(ctx, call)
		}()

		select {
		case result := <-done:
			return toolResultMsg{gen: gen, index: index, result: result, modeSwitch: req}
		case <-ctx.Done():
			return toolResultMsg{
				gen:   gen,
				index: index,
				result: api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: tool %q timed out after %s. Treat this as a stuck call: do not retry the same arguments blindly; inspect state, use a narrower command or shorter timeout, and continue with another approach.", call.Function.Name, timeout),
				},
				modeSwitch: nil,
			}
		}
	}
}

func (m *Model) processPendingTools() tea.Cmd {
	if m.pending == nil {
		return nil
	}
	// Pre-warm the stale-edit ledger on the update goroutine so its lazy init
	// can't race once the batch's tool goroutines start (see invokeTool).
	m.freshnessLedger()
	// A question is a conversation boundary, not one operation in a parallel
	// batch. Run only the first ask_user call and cancel every other call that has
	// not started, so nothing can continue behind the user's back while the model
	// claims to be waiting for an answer.
	questionIndex := -1
	for i, call := range m.pending.calls {
		if call.Function.Name == "ask_user" {
			questionIndex = i
			break
		}
	}
	if questionIndex >= 0 {
		for i, call := range m.pending.calls {
			if i == questionIndex || m.pending.started[i] {
				continue
			}
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  "not run because ask_user requires the model to stop and wait for the user's answer.",
			}
			m.pending.started[i] = true
			m.pending.done++
		}
	}

	if m.pending.done >= len(m.pending.calls) {
		batchCalls := m.pending.calls
		batchResults := m.pending.results
		deniedTool := m.pending.deniedTool
		m.history = append(m.history, batchResults...)
		// The batch is now whole, so anything held back to avoid splicing into
		// it (an /undo typed mid-turn) can land here.
		m.flushDeferredAdvisory()
		m.noteFetchedContent(batchCalls, batchResults)
		m.pending = nil
		m.markToolsDone()

		// A denial is a user decision, not another recoverable tool failure. End
		// the turn here instead of handing the result back to the model and giving
		// it an opportunity to rephrase and re-request the same action.
		if deniedTool != "" {
			m.denialFeedbackTool = deniedTool
			m.history = append(m.history, api.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("I stopped after you denied %s. What should I change, or why did you want that call denied? I won't request it again while handling your reply.", deniedTool),
			})
			m.pauseForUser("stopped after denial — waiting for your feedback", "permission_denied")
			m.finalizeCheckpoint(m.lastUserMessage())
			m.finishTurnClock()
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return nil
		}
		if questionIndex >= 0 {
			m.markPlanPresented()
			// A question with options becomes a picker: the model gets back one of
			// its own labels instead of prose it has to interpret, which is the
			// difference between a decided turn and another clarifying round.
			m.questionResult = -1
			if q := tools.ParseAskUser(batchCalls[questionIndex].Function.Arguments); len(q.Options) > 0 {
				q.Options = orderedQuestionOptions(q)
				m.question = q
				m.questionCursor = 0
				m.questionChecked = nil
				// The batch's results were appended to history above; remember
				// where this call's placeholder landed so a picked answer can
				// rewrite it as the tool result (see applyQuestionAnswer).
				m.questionResult = len(m.history) - len(batchResults) + questionIndex
				m.promptState(stateQuestion)
			}
			m.pauseForUser("waiting for your answer", "awaiting_user_answer")
			m.finalizeCheckpoint(m.lastUserMessage())
			m.finishTurnClock()
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return nil
		}

		madeProgress, warnOscillation, stopOscillation := m.observeRoundProgress(batchCalls, batchResults)
		warnStagnant, stopStagnant := m.observeStagnation(madeProgress)

		// Outcomes include both the calls and their results, so A/B/A/B only
		// trips when the observable evidence is repeating, not merely when the
		// model uses the same pair of tools productively.
		if warnOscillation && !m.oscillationWarned {
			m.history = append(m.history, advisory("[NO PROGRESS DETECTED] You are alternating between the same actions and receiving the same results. Stop, state your blocker explicitly, and try a materially different approach."))
			m.oscillationWarned = true
		}
		if warnStagnant {
			m.history = append(m.history, advisory("[NO PROGRESS DETECTED] Your last three rounds of tool calls returned nothing this turn has not already seen. Use the evidence you have, take a materially different action, or state the blocker."))
		}

		// Inspection calls include arguments in their repeat identity, so reading
		// different files or running different searches is progress. Mutation and
		// control tools remain name-based to catch varied-argument spam.
		// The stagnation guard speaks for the same round when one tool repeats,
		// and it is the stronger statement: telling the model the turn is over and
		// then promising it a next message in the very next line is how a small
		// model talks itself back into the loop. Whichever fires, only one speaks.
		batchTool, warnRepeat, stopRepeat, announceStop := m.observeRepeatedBatch(batchCalls, madeProgress)
		if warnRepeat && !warnStagnant {
			m.history = append(m.history, advisory(fmt.Sprintf("[REPEATING ACTION] You have called %q %d times in a row without making progress. Stop repeating it — take a different action, or if you're blocked, explain the blocker to the user in plain text.", batchTool, m.sameToolStreak)))
		}

		// A detector stop used to end the turn on the spot. Interactive, the
		// human gets the call first: the escalation parks here (no stream is
		// running, the batch's results are already in history) and the modal
		// decides whether applyLoopGuardStops runs at all. Auto mode and a
		// detector past its continue cap keep the automatic stop.
		if kind, tool := loopStopKind(stopOscillation, stopStagnant, stopRepeat, batchTool); kind != "" {
			esc := &loopEscalation{
				kind:            kind,
				tool:            tool,
				streak:          m.sameToolStreak,
				rounds:          m.stagnantRounds,
				stopOscillation: stopOscillation,
				stopStagnant:    stopStagnant,
				stopRepeat:      stopRepeat,
				announceStop:    announceStop,
			}
			if m.shouldAskLoopEscalation(kind) {
				m.loopEscalation = esc
				m.loopGuardCursor = 0
			} else {
				m.applyLoopGuardStops(esc)
			}
		}

		// Re-read guard: the streak guard above resets on any interleaved call,
		// so it misses a model re-reading files it already has. Re-reading a
		// file nothing has mutated is always wasted work.
		if rereads, stopRereads := m.observeFileReads(batchCalls); len(rereads) > 0 {
			m.history = append(m.history, advisory(fmt.Sprintf("[RE-READ DETECTED] You already read \"%s\" this turn and nothing has changed it since — you have the contents. Use them, or grep for the specific thing you need instead of re-reading whole files.", strings.Join(rereads, `", "`))))
			if stopRereads {
				if !m.rereadStopAnnounced {
					m.rereadStopAnnounced = true
					m.history = append(m.history, advisory("[LOOP BROKEN] You keep re-reading files you already have the contents of. Tools are disabled for your next message — answer the user in plain text with what you know."))
				}
				m.stopForBlockerReport()
			}
		}

		// Step budget: cap tool-call rounds per user turn so a confused model
		// can't loop forever burning tokens.
		m.stepCount++
		limit := m.turnStepLimit()
		if m.mode == AutoMode {
			limit = autoModeMaxSteps
		}
		if m.stepCount >= limit {
			m.history = append(m.history, advisory("[STEP BUDGET EXHAUSTED] You have used your tool-call budget for this turn. Stop calling tools: summarize what you did, what remains, and ask the user how to proceed."))
			m.stopForBlockerReport()
		}

		// The loop-guard escalation parks the turn here: no stream is running
		// and history is complete through this batch, so nothing moves until
		// the user answers the modal (see updateLoopGuard). If another guard
		// already ended the turn on the way down (re-read cap, step budget),
		// asking is moot — the parked stop lands automatically alongside it.
		if m.loopEscalation != nil {
			if m.turnStoppedByGuard {
				esc := m.loopEscalation
				m.loopEscalation = nil
				m.applyLoopGuardStops(esc)
			} else {
				m.promptState(stateLoopGuard)
				m.toast = "agent appears stuck"
				m.refreshTranscript()
				m.viewport.GotoBottom()
				return nil
			}
		}

		cmd := m.startStream()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return cmd
	}

	var cmds []tea.Cmd
	inFlight := 0
	for i, started := range m.pending.started {
		if started && i < len(m.pending.results) && m.pending.results[i].Role == "" {
			inFlight++
		}
	}
	parallelLimit := m.parallelToolLimit()
	for i, call := range m.pending.calls {
		if m.pending.started[i] {
			continue
		}

		// A banned tool is gone from the schema, but the text-form fallback parses
		// tool calls straight out of assistant prose, so the schema alone does not
		// hold the ban. Refuse it here — the one path every call goes through —
		// and answer with a tool result so the transcript stays well-formed.
		if m.bannedTools[call.Function.Name] {
			m.failedCalls[tools.CallFingerprint(call)]++
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: %q is disabled for the rest of this turn — you called it repeatedly without making progress. Do not call it again; act on what you already know or answer the user.", call.Function.Name),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		if !m.toolCallAllowedInMode(call) {
			m.failedCalls[tools.CallFingerprint(call)]++
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: tool %q not allowed in %s mode (press shift+tab to switch modes)", call.Function.Name, m.mode),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		// terminal_open joins run_shell here as defence in depth, not as the
		// boundary. The boundary is ModeMutable in tools/policy.go, and it is
		// what has to hold, because this preflight is TUI-only: the sub-agent
		// path filters by name through toolAllowedInMode and never reaches this
		// code, and internal/agent's executor does no mode gating at all. An
		// allowlist could not hold a terminal anyway — it vets the opening
		// command, and terminal_send types whatever it likes afterwards, which
		// is why no allowlist is applied to terminal_send's input: a token
		// filter on live REPL keystrokes buys false confidence. This is here so
		// that if the policy is ever widened to ModeExploreShell the allowlist
		// applies automatically instead of silently not existing.
		if call.Function.Name == "run_shell" || call.Function.Name == "terminal_open" {
			cmd := safeshell.ExtractShellCommand(call.Function.Arguments)

			// Explore-mode read-only allowlist (per-segment bin/sub check).
			if m.mode == ExploreMode {
				if ok, reason := safeshell.IsExploreReadOnlyShell(cmd); !ok {
					m.failedCalls[tools.CallFingerprint(call)]++
					m.pending.results[i] = api.Message{
						Role:     "tool",
						ToolName: call.Function.Name,
						Content:  fmt.Sprintf("error: %s. Call switch_mode(\"plan\", ...) and then switch_mode(\"write\", ...) to run mutating commands.", reason),
					}
					m.pending.started[i] = true
					m.pending.done++
					continue
				}
			}

			// VCS bypass guard (all modes): in an ivaldi repo, reject bare
			// `git` invocations that would bypass the MCP translation layer
			// and fail with "not a git repository". The git_* tools translate
			// transparently; raw `git` via run_shell does not.
			if ok, reason := safeshell.InterceptVCSBypass(cmd, tools.DetectVCS()); !ok {
				m.failedCalls[tools.CallFingerprint(call)]++
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  "error: " + reason,
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
		}

		if call.Function.Name == "switch_mode" {
			req, err := parseModeSwitchArgs(call.Function.Arguments)
			switch {
			case err != nil:
				// Genuinely malformed (bad/unknown mode) — report and move on.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: %v", err),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case req.target == AutoMode:
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  "error: transition to 'auto' mode can only be triggered by the user explicitly, not via tool call.",
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case m.planGateBlocks(req.target):
				// The plan is the handoff. Write mode may run on a different,
				// smaller model, and even when it doesn't, notes are re-injected
				// every turn while chat history gets truncated away. Deliberately
				// not counted as a failed call: the model is meant to write the
				// notes and retry this exact call.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  m.planGateMessage(),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case req.target == m.mode:
				// Redundant switch: succeed as a no-op rather than erroring, so a
				// confused model doesn't spin retrying the same switch.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("already in %s mode", m.mode),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
			// Any real transition (forward or backward) is allowed; it's applied
			// when the result returns (toolResultMsg -> applyModeTransition).
			// Backward switches go to a safer/more-restrictive mode; permission
			// prompts still gate destructive tools in write mode.
		}

		// Enforced half of plan verification: a file the plan named cannot be
		// edited until it has been read this turn. The handoff message asks for
		// this; a small local model may ignore a prompt, but not this.
		// (The stale-edit guard no longer needs a preflight here: the registry
		// refuses drifted mutations inside dispatch — see tools/freshness.go.)
		if reason := m.requireReadBeforeEdit(call.Function.Name, tools.MutatedPaths(call.Function.Name, call.Function.Arguments)); reason != "" {
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  reason,
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		// Short-circuit a call that has already failed identically: re-running
		// it won't help and just burns a round-trip.
		fp := tools.CallFingerprint(call)
		if m.failedCalls[fp] >= maxSameCallFailures {
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: you already called %q with these exact arguments %d times and it failed each time. Do not repeat it — change the arguments or use a different approach.", call.Function.Name, m.failedCalls[fp]),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		// Config permission rules. A deny rule is absolute: it rejects here,
		// before any prompt and before the batch-wide "allow all" the permission
		// modal can set, so a rule the user wrote cannot be waived by a key they
		// pressed for a different call.
		if effect, ok := tools.EvaluatePermission(m.cfg.Permissions, call); ok && effect == tools.PermissionDeny {
			m.recordPermission(call, "denied_by_rule")
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  "denied by a permission rule in the user's config. Do NOT retry this call or a minor variant — take a different approach, or tell the user which rule is in your way.",
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		// Explore-mode run_shell calls are prechecked above and are read-only,
		// so they don't need a permission prompt.
		exploreReadOnly := m.mode == ExploreMode && call.Function.Name == "run_shell"
		if m.shouldPromptPermission(call) && !exploreReadOnly {
			m.pending.index = i
			m.pending.preview = computePreview(call)
			// When the fetched-content gate is what stands between this call
			// and execution, say so — y/N is then a decision about whether the
			// action is the user's intent or an injected instruction.
			if m.mode == AutoMode && m.fetchedContent {
				m.pending.preview = "Web content entered the conversation this turn — confirm this action is your intent, not an injected instruction.\n" + m.pending.preview
			}
			// Name the model the switch would route to, so y/N is a decision
			// about which model runs next, not just which mode.
			if call.Function.Name == "switch_mode" {
				if req, err := parseModeSwitchArgs(call.Function.Arguments); err == nil {
					m.pending.preview += m.routeNote(req.target)
				}
			}
			m.promptState(statePermission)
			m.refreshTranscript()
			break
		}

		if inFlight >= parallelLimit {
			break
		}
		m.pending.started[i] = true
		inFlight++
		cmds = append(cmds, m.invokeToolCmd(m.pending.gen, i, call))
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	// Mode/preflight failures above complete synchronously and therefore do not
	// produce a toolResultMsg to re-enter this method. Finalize the batch now;
	// otherwise a batch made entirely of rejected calls remains stuck forever
	// at TOOLS n/n.
	if m.pending != nil && m.pending.done >= len(m.pending.calls) {
		return m.processPendingTools()
	}

	return nil
}

func (m *Model) toolCallAllowedInMode(call tools.ToolCall) bool {
	if !m.toolAllowedInMode(call.Function.Name) {
		return false
	}
	if m.mode == WriteMode || m.mode == AutoMode {
		return true
	}
	// These combined inspect/mutate tools are visible in read-only modes for
	// their default list/show actions. Argument-aware gating prevents a create,
	// delete, add, or remove action from slipping through that name-level policy.
	var args struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(call.Function.Arguments, &args)
	switch call.Function.Name {
	case "git_branch":
		return args.Action == "" || args.Action == "list"
	case "git_remote":
		return args.Action == "" || args.Action == "list" || args.Action == "show"
	default:
		return true
	}
}

func computePreview(call tools.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Function.Arguments, &args)

	switch call.Function.Name {
	case "switch_mode":
		mode, _ := args["mode"].(string)
		reason, _ := args["reason"].(string)
		return fmt.Sprintf("Switch mode to: %s\nReason: %s", strings.TrimSpace(mode), strings.TrimSpace(reason))
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		// write_file formats before it writes, so an unformatted preview would
		// not be the content the user is approving.
		content = string(tools.FormatPreview(path, []byte(content)))
		old, err := os.ReadFile(path)
		if err != nil {
			return "(new file " + path + ")\n" + addedLines(truncatePreview(content, 20))
		}
		return simpleDiff(string(old), content, 10)
	case "edit_file":
		path, _ := args["path"].(string)
		if diff, ok := tools.PreviewEdit(path, call.Function.Arguments); ok {
			return diff
		}
		// The edit could not be resolved against the file (no match, unreadable);
		// the model's claim is still more than an empty modal.
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		return path + "\n" + simpleDiff(oldStr, newStr, 3)
	case "parallel_edit":
		tasks, _ := args["tasks"].([]any)
		var b strings.Builder
		fmt.Fprintf(&b, "parallel_edit: %d subtask(s), each applied through the write path\n", len(tasks))
		for i, t := range tasks {
			tm, _ := t.(map[string]any)
			task, _ := tm["task"].(string)
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncatePreview(strings.TrimSpace(task), 3))
			var files []string
			fl, _ := tm["files"].([]any)
			for _, f := range fl {
				if s, ok := f.(string); ok {
					files = append(files, s)
				}
			}
			if len(files) == 0 {
				b.WriteString("   files: not declared — this worker may touch any file\n")
				continue
			}
			fmt.Fprintf(&b, "   files: %s\n", strings.Join(files, ", "))
		}
		return strings.TrimRight(b.String(), "\n")
	case "append_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		label := path + " — appended:"
		if _, err := os.Stat(path); err != nil {
			label = "(new file " + path + ") — appended:"
		}
		return label + "\n" + addedLines(truncatePreview(content, 15))
	case "delete_file":
		path, _ := args["path"].(string)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Sprintf("%s (not found)", path)
		}
		var preview strings.Builder
		preview.WriteString(fmt.Sprintf("%s (%d bytes)\n", path, info.Size()))
		if !info.IsDir() {
			data, _ := os.ReadFile(path)
			lines := strings.Split(string(data), "\n")
			for i := 0; i < 3 && i < len(lines); i++ {
				preview.WriteString(lines[i] + "\n")
			}
			if len(lines) > 3 {
				preview.WriteString("...")
			}
		}
		return preview.String()
	case "move_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("move %s → %s", src, dst)
	case "copy_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("copy %s → %s", src, dst)
	case "run_shell":
		cmd, _ := args["command"].(string)
		return fmt.Sprintf("shell: %s", cmd)
	case "terminal_open":
		cmd, _ := args["command"].(string)
		if strings.TrimSpace(cmd) == "" {
			cmd = "(default shell)"
		}
		return fmt.Sprintf("open terminal: %s", cmd)
	case "terminal_send":
		id, _ := args["id"].(float64)
		input, _ := args["input"].(string)
		return fmt.Sprintf("terminal %d ← %s", int(id), truncatePreview(input, 10))
	case "git_add":
		paths, _ := args["paths"].(string)
		return fmt.Sprintf("git add %s", paths)
	case "git_commit":
		msg, _ := args["message"].(string)
		return fmt.Sprintf("git commit -m %q", msg)
	default:
		return ""
	}
}

// addedLines prefixes each line with '+' so a preview of new/appended content
// colorizes as additions in the permission modal.
func addedLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "+" + l
	}
	return strings.Join(lines, "\n")
}

func truncatePreview(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n..."
}

func simpleDiff(old, new string, context int) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")
	// Very naive diff: find first changed line and last changed line
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	endOld := len(oldLines) - 1
	endNew := len(newLines) - 1
	for endOld >= start && endNew >= start && oldLines[endOld] == newLines[endNew] {
		endOld--
		endNew--
	}
	ctxStart := max(start-context, 0)
	ctxEndOld := endOld + context
	if ctxEndOld >= len(oldLines) {
		ctxEndOld = len(oldLines) - 1
	}
	ctxEndNew := endNew + context
	if ctxEndNew >= len(newLines) {
		ctxEndNew = len(newLines) - 1
	}
	var b strings.Builder
	if ctxStart > 0 {
		b.WriteString("...\n")
	}
	for i := ctxStart; i <= ctxEndOld && i < len(oldLines); i++ {
		if i >= start && i <= endOld {
			fmt.Fprintf(&b, "-%s\n", oldLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", oldLines[i])
		}
	}
	for i := ctxStart; i <= ctxEndNew && i < len(newLines); i++ {
		if i >= start && i <= endNew {
			fmt.Fprintf(&b, "+%s\n", newLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", newLines[i])
		}
	}
	if ctxEndNew < len(newLines)-1 || ctxEndOld < len(oldLines)-1 {
		b.WriteString("...")
	}
	return b.String()
}

// noteFetchedContent records when untrusted web bytes actually entered the
// conversation this turn (see webContentTools). A failed fetch returns an
// error string without the untrusted markers and does not trip the gate.
func (m *Model) noteFetchedContent(calls []tools.ToolCall, results []api.Message) {
	if m.fetchedContent {
		return
	}
	for i, call := range calls {
		if !webContentTools[call.Function.Name] || i >= len(results) {
			continue
		}
		if strings.Contains(results[i].Content, "UNTRUSTED EXTERNAL CONTENT") {
			m.fetchedContent = true
			return
		}
	}
}

func (m *Model) shouldPromptPermission(call tools.ToolCall) bool {
	// Config rules answer first, so an allow rule spares the prompt for a call
	// the user has already blessed, and an ask rule can pull a non-destructive
	// tool into the prompt. Deny is handled before this point (it must outrank
	// allowAll) and reaching it here would mean that check was bypassed — prompt,
	// which is the fail-safe answer.
	if effect, ok := tools.EvaluatePermission(m.cfg.Permissions, call); ok {
		return effect != tools.PermissionAllow
	}
	if m.pending.allowAll {
		return false
	}
	policy := tools.PolicyForName(call.Function.Name)
	if m.tools != nil {
		if tool, ok := m.tools.Lookup(call.Function.Name); ok {
			policy = tool.Policy
		}
	}
	if !policy.Destructive {
		return false
	}
	if m.mode == AutoMode {
		// Fetched-content gate: once untrusted web bytes have entered the
		// conversation this turn, any destructive call may be carrying out an
		// injected instruction — confirm with the user even for in-workspace
		// paths. (Write mode already prompts for every destructive call.)
		if m.fetchedContent {
			return true
		}
		var args map[string]any
		if err := json.Unmarshal(call.Function.Arguments, &args); err == nil {
			for _, key := range []string{"path", "dest", "destination", "new_path", "to", "source", "src", "working_dir"} {
				if p, ok := args[key].(string); ok && p != "" {
					if !m.isPathInTrustedFolder(p) {
						return true
					}
				}
			}
		}
		return false
	}
	// For all other modes (Explore, Plan, Write), prompt for all destructive tools
	return true
}

func (m *Model) isPathInTrustedFolder(targetPath string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	var absTarget string
	if filepath.IsAbs(targetPath) {
		absTarget = filepath.Clean(targetPath)
	} else {
		absTarget = filepath.Clean(filepath.Join(absCwd, targetPath))
	}
	rel, err := filepath.Rel(absCwd, absTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
