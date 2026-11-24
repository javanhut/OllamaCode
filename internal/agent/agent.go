// Package agent provides a minimal, non-streaming agent loop reused by the
// in-session sub-agent tool and the eval harness. It deliberately mirrors the
// TUI loop's safety posture (step cap, content-parse fallback, tool filtering)
// but without any UI or streaming.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/internal/verification"
	"github.com/javanhut/ollama_code/tools"
)

// ChatClient is the subset of api.OllamaHost the loop needs; an interface so the
// loop can be unit-tested with a fake. api.OllamaHost satisfies it.
type ChatClient interface {
	ChatOnce(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error)
}

// Options configures a headless run.
type Options struct {
	Model             string
	System            string
	MaxSteps          int                    // tool-call rounds before giving up (default 8)
	NumCtx            int                    // num_ctx option, if > 0
	ToolFilter        func(name string) bool // which tools the agent may see/call (nil = all)
	Trace             *tracepkg.Recorder     // optional redacted JSONL recorder
	StructuredResults *bool                  // nil/true=envelopes; false for A/B evaluation
	// ConstrainToolCalls asks for first-pass schema-constrained tool output
	// (small-tier models on native Ollama; see constrain.go). The format repair
	// path stays as the fallback for argument-level mistakes.
	ConstrainToolCalls bool
	// Constraints carries the per-model+host rung cache across runs (a parent
	// shares its cache with spawned sub-agents); nil uses a throwaway cache so
	// the fallback ladder still works within this run.
	Constraints *ConstraintCache
	// Before, when set, is forwarded to the executor and runs synchronously
	// before each dispatched tool call (see Executor.Before). The TUI installs
	// it to checkpoint a child's file-mutating calls into the PARENT turn's
	// /undo bank, so one undo rewinds a whole delegation. Nil means no hook.
	Before func(tools.ToolCall)
	// Permissions forwards the user's configured rules to the executor, so a
	// deny holds inside a headless run and inside a spawned subagent, not only
	// at the interactive prompt.
	Permissions []tools.PermissionRule
	// PriorMessages, when set, seeds the run with a prior conversation — a
	// finished sub-agent's retained history (see Result.Messages) so the
	// parent can send it a follow-up. The seed is expected to lead with its
	// own system message, so System is not prepended again; task is appended
	// as the next user message. The slice is copied before use.
	PriorMessages []api.Message
	// OnAssistant, when set, observes every model reply as it arrives: the
	// prose content plus the tool calls it made (nil for the final answer,
	// including the forced synthesis after a step-limit stop). Headless
	// stream-json output is built on it. It must not block for long.
	OnAssistant func(content string, calls []tools.ToolCall)
	// OnToolResult, when set, observes each tool call's outcome in dispatch
	// order — including calls the loop refused (filtered or repeated) — with
	// failed=true when the call errored or its command exited nonzero.
	OnToolResult func(call tools.ToolCall, result string, failed bool)
	// AugmentResult, when set, may rewrite a successfully dispatched tool's
	// result before the model sees it (e.g. to attach instruction files the
	// call just made relevant). Returning result unchanged is a no-op.
	AugmentResult func(call tools.ToolCall, result string) string
	// Verify runs the project's verification plan (see internal/verification:
	// targeted tests plus a build, derived from known manifests) whenever the
	// model tries to finish after changing files. A failure goes back to the
	// model instead of ending the run, so "done" means the code builds.
	Verify bool
	// VerifyCmd overrides the detected plan with an explicit command.
	VerifyCmd string
}

// Result is the outcome of a headless run.
type Result struct {
	Output           string // the model's final (non-tool) message
	Steps            int    // tool-call rounds executed
	HitLimit         bool   // true if MaxSteps was reached without a final answer
	ToolsUsed        []string
	ToolCalls        int
	ToolErrors       int
	ArgumentFailures int
	RepairAttempts   int
	RepairsSucceeded int
	RepeatedBlocked  int
	PromptTokens     int
	CompletionTokens int
	// Verification is the gate's verdict on the files this run changed:
	// "passed", "failed" (still failing when the retry budget ran out), or ""
	// when nothing was checked (no changes, no plan, or Verify off).
	Verification   string
	VerifyFailures int // failed checks handed back to the model
	// Messages is the run's full conversation — any PriorMessages seed, this
	// run's exchanges, and the final assistant answer — so the caller can
	// retain it and later resume the child with a follow-up (see
	// Options.PriorMessages). It is set on error too, holding the partial
	// history up to the failure or interruption.
	Messages []api.Message
}

// Loop-safety tunables for the headless agent.
const (
	defaultMaxSteps   = 8
	maxIdenticalCalls = 2 // dispatch an identical call at most this many times before refusing it
	maxVerifyAttempts = 3 // failed verifications handed back before the run ends on a failing check
)

// Run executes a bounded agent loop: prompt the model, dispatch any tool calls
// (native or parsed from content), feed results back, repeat until the model
// answers without calling tools or a guard trips. It mirrors the TUI loop's
// safety posture — per-call timeout + panic recovery, JSON salvage, constrained-
// decoding escalation, repeated-call and oscillation detection — and, rather
// than dead-ending on the step cap, forces a final tool-less synthesis so
// partial findings are always returned.
func Run(ctx context.Context, host ChatClient, reg *tools.Registry, task string, opts Options) (Result, error) {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxSteps
	}
	// One stale-edit ledger per run, deliberately NOT inherited from the
	// caller's context: agents share the filesystem but not observations, so
	// a sub-agent must read a file itself before its writes are fresh.
	ctx = tools.WithFreshnessLedger(ctx, tools.NewFreshnessLedger())
	defs := filterTools(reg.Definitions(), opts.ToolFilter)
	if opts.Trace != nil {
		names := make([]string, 0, len(defs))
		for _, definition := range defs {
			names = append(names, definition.Function.Name)
		}
		_ = opts.Trace.Record(tracepkg.Event{Kind: "turn_start", Model: opts.Model, Metadata: map[string]any{"tools": names, "task": task}})
	}
	options := map[string]any{}
	if opts.NumCtx > 0 {
		options["num_ctx"] = opts.NumCtx
	}

	var msgs []api.Message
	if len(opts.PriorMessages) > 0 {
		// A follow-up to a finished child: the seed already leads with the
		// system prompt, so don't prepend System again.
		msgs = append(msgs, opts.PriorMessages...)
	} else if opts.System != "" {
		msgs = append(msgs, api.Message{Role: "system", Content: opts.System})
	}
	msgs = append(msgs, api.Message{Role: "user", Content: task})

	var res Result
	executor := Executor{Registry: reg, Host: host, Model: opts.Model, NumCtx: opts.NumCtx, StructuredResults: opts.StructuredResults,
		Before: opts.Before, Permissions: opts.Permissions,
		Observe: func(event ExecutionEvent) {
			if opts.Trace == nil {
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
			_ = opts.Trace.Record(tracepkg.Event{Kind: "tool", Model: opts.Model, Tool: event.Call.Function.Name,
				Arguments: event.Call.Function.Arguments, Result: event.Result, Error: errText,
				DurationMS: event.Duration.Milliseconds(), Metadata: meta})
		},
	}
	fpCount := map[string]int{} // call fingerprint -> times dispatched
	var guard LoopGuard         // same repeat/oscillation/stagnation/re-read detectors as the TUI
	guard.Reset()
	changed := map[string]bool{} // files mutated this run, for the verification gate

	// First-pass constrained decoding is opted into by callers that know the
	// model's tier; the loop additionally requires a native-Ollama host and a
	// non-empty tool list (a tool-less request is a prose turn).
	var constraints *ConstraintCache
	constraintKey := ""
	if opts.ConstrainToolCalls && ConstrainedDecodingSupported(host) {
		constraints = opts.Constraints
		if constraints == nil {
			constraints = NewConstraintCache()
		}
		constraintKey = ConstraintKey(host, opts.Model)
	}

	for res.Steps < opts.MaxSteps {
		req := api.ChatRequest{
			Model:    opts.Model,
			Messages: msgs,
			Tools:    defs,
			Options:  options,
		}
		constrained := false
		if constraints != nil {
			req.Format, constrained = constraints.Format(constraintKey, defs)
		}
		recordModelRequest(opts.Trace, opts.Model, req, constrained)
		resp, err := host.ChatOnce(ctx, req)
		// A host that rejects the schema (400 from the grammar conversion) gets
		// an immediate retry at the next-weaker rung; the cache starts later
		// requests at the working rung, so the probe cost is paid once.
		for err != nil && constrained && IsFormatRejection(err) && constraints.Downgrade(constraintKey) {
			req.Format, constrained = constraints.Format(constraintKey, defs)
			recordModelRequest(opts.Trace, opts.Model, req, constrained)
			resp, err = host.ChatOnce(ctx, req)
		}
		if err != nil {
			if opts.Trace != nil {
				_ = opts.Trace.Record(tracepkg.Event{Kind: "model_error", Model: opts.Model, Error: err.Error()})
			}
			// Keep the partial history so an interrupted or failed child can
			// still be resumed from where it stopped.
			res.Messages = msgs
			return res, err
		}
		if opts.Trace != nil {
			payload, _ := json.Marshal(resp)
			_ = opts.Trace.Record(tracepkg.Event{Kind: "model_response", Model: opts.Model, Payload: payload})
		}
		res.PromptTokens += resp.PromptEval
		res.CompletionTokens += resp.EvalCount
		calls := resp.Message.ToolCalls
		if len(calls) == 0 {
			calls = reg.ParseToolCallsFromContent(resp.Message.Content)
			if len(calls) > 0 && opts.Trace != nil {
				_ = opts.Trace.Record(tracepkg.Event{Kind: "tool_calls_parsed_from_content", Model: opts.Model,
					Metadata: map[string]any{"received": len(calls), "calls": calls}})
			}
		}
		if len(calls) == 0 {
			output := resp.Message.Content
			// A constrained reply that isn't a tool call chose the prose escape
			// branch; unwrap it so callers see the answer, not the envelope.
			if constrained {
				if prose, ok := UnwrapConstrainedProse(output); ok {
					output = prose
				}
			}
			if feedback, retry := verifyGate(ctx, opts, changed, &res); retry {
				msgs = append(msgs, api.Message{Role: "assistant", Content: output},
					api.Message{Role: "user", Advisory: true, Content: feedback})
				if opts.OnAssistant != nil {
					opts.OnAssistant(output, nil)
				}
				continue
			}
			res.Output = output
			res.Messages = append(msgs, api.Message{Role: "assistant", Content: output})
			if opts.OnAssistant != nil {
				opts.OnAssistant(output, nil)
			}
			if opts.Trace != nil {
				_ = opts.Trace.Record(tracepkg.Event{Kind: "turn_end", Model: opts.Model, Metadata: map[string]any{"reason": "completed", "steps": res.Steps, "prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens}})
			}
			return res, nil
		}
		rawCalls := calls
		calls = tools.DedupeCalls(rawCalls)
		if len(calls) != len(rawCalls) && opts.Trace != nil {
			_ = opts.Trace.Record(tracepkg.Event{Kind: "tool_calls_deduplicated", Model: opts.Model,
				Metadata: map[string]any{"received": len(rawCalls), "kept": len(calls), "calls": rawCalls}})
		}
		res.ToolCalls += len(calls)

		res.Steps++
		msgs = append(msgs, api.Message{Role: "assistant", Content: resp.Message.Content, ToolCalls: calls})
		if opts.OnAssistant != nil {
			opts.OnAssistant(resp.Message.Content, calls)
		}
		// toolMsg appends a tool result and reports it to the observer, so
		// every call the model made gets exactly one OnToolResult.
		toolMsg := func(c tools.ToolCall, content string, failed bool) {
			msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name, Content: content})
			if opts.OnToolResult != nil {
				opts.OnToolResult(c, content, failed)
			}
		}

		roundStart := len(msgs)
		for _, c := range calls {
			res.ToolsUsed = append(res.ToolsUsed, c.Function.Name)
			if opts.ToolFilter != nil && !opts.ToolFilter(c.Function.Name) {
				res.ToolErrors++
				toolMsg(c, "error: tool not permitted for this agent", true)
				continue
			}
			if guard.BannedTools[c.Function.Name] {
				res.RepeatedBlocked++
				toolMsg(c, BannedToolRefusal(c.Function.Name), true)
				continue
			}
			fp := tools.CallFingerprint(c)
			// Stuck-guard: refuse an identical call the model keeps re-issuing
			// rather than re-running it, so a weak model can't burn the budget
			// looping on one action.
			if fpCount[fp] >= maxIdenticalCalls {
				res.RepeatedBlocked++
				toolMsg(c, fmt.Sprintf("error: you already ran this exact call %d times with the same result. Stop repeating it — use what you already have, or take a materially different action.", fpCount[fp]), true)
				continue
			}
			fpCount[fp]++

			// Same tool-call robustness as the TUI loop: salvage almost-valid JSON,
			// run with a per-call timeout + panic recovery, escalate argument
			// errors to constrained decoding, and feed back actionable hints.
			event := executor.Execute(ctx, c)
			if event.ArgumentFailure {
				res.ArgumentFailures++
			}
			if event.RepairAttempted {
				res.RepairAttempts++
			}
			if event.RepairSucceeded {
				res.RepairsSucceeded++
			}
			if event.Err != nil {
				res.ToolErrors++
			}
			result := event.Result
			if opts.AugmentResult != nil && event.Err == nil {
				result = opts.AugmentResult(event.Call, result)
			}
			toolMsg(event.Call, result, event.Err != nil || event.ExitCode != 0)
			if event.Err == nil {
				if mutated := tools.MutatedPaths(event.Call.Function.Name, event.Call.Function.Arguments); len(mutated) > 0 {
					guard.ForgetReads(mutated) // re-reading a just-changed file is legitimate
					for _, p := range mutated {
						changed[p] = true
					}
					res.Verification = "" // new changes void an earlier pass
				}
			}
		}

		if stop, advisories := observeRound(&guard, calls, msgs[roundStart:]); stop {
			break
		} else {
			for _, text := range advisories {
				msgs = append(msgs, api.Message{Role: "user", Advisory: true, Content: text})
			}
		}
	}

	// Didn't answer on its own: force one tool-less pass so partial findings come
	// back instead of a useless "hit the limit" sentinel.
	res.HitLimit = true
	output, history, promptTokens, completionTokens := finalize(ctx, host, opts, options, msgs, verifyBeforeReport(ctx, opts, changed, &res))
	res.Output = output
	res.Messages = history
	if opts.OnAssistant != nil {
		opts.OnAssistant(output, nil)
	}
	res.PromptTokens += promptTokens
	res.CompletionTokens += completionTokens
	if opts.Trace != nil {
		_ = opts.Trace.Record(tracepkg.Event{Kind: "turn_end", Model: opts.Model, Metadata: map[string]any{"reason": "limit_or_loop_guard", "steps": res.Steps, "prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens}})
	}
	return res, nil
}

// finalize asks the model, with NO tools available, to write up whatever it
// gathered. Passing no tools forces a prose answer rather than another tool
// call. It also returns the full history — the loop's messages plus the
// advisory nudge and the model's reply — so the caller can retain the child's
// conversation for a later follow-up.
func finalize(ctx context.Context, host ChatClient, opts Options, options map[string]any, msgs []api.Message, verdict string) (string, []api.Message, int, int) {
	// Advisory: the harness wrote this, not the user. Without the flag the trace
	// exporter reads it as the sub-agent's task and every limit-hitting
	// trajectory trains on the nudge instead of the real prompt.
	content := "Stop. Do NOT call any more tools. Based on everything above, write your final report now: a direct answer to the task plus the concrete file paths, line references, and commands you used. If you couldn't finish, say what you found and what remains."
	if verdict != "" {
		content += "\n\n" + verdict
	}
	msgs = append(msgs, api.Message{Role: "user", Advisory: true, Content: content})
	req := api.ChatRequest{
		Model:    opts.Model,
		Messages: msgs,
		Options:  options,
	}
	recordModelRequest(opts.Trace, opts.Model, req, false)
	resp, err := host.ChatOnce(ctx, req)
	if err != nil || strings.TrimSpace(resp.Message.Content) == "" {
		if err != nil && opts.Trace != nil {
			_ = opts.Trace.Record(tracepkg.Event{Kind: "model_error", Model: opts.Model, Error: err.Error(), Metadata: map[string]any{"finalize": true}})
		}
		return "(sub-agent stopped without a final answer)", msgs, 0, 0
	}
	if opts.Trace != nil {
		payload, _ := json.Marshal(resp)
		_ = opts.Trace.Record(tracepkg.Event{Kind: "model_response", Model: opts.Model, Payload: payload})
	}
	msgs = append(msgs, api.Message{Role: "assistant", Content: resp.Message.Content})
	return resp.Message.Content, msgs, resp.PromptEval, resp.EvalCount
}

func recordModelRequest(recorder *tracepkg.Recorder, model string, req api.ChatRequest, constrained bool) {
	if recorder == nil {
		return
	}
	names := make([]string, 0, len(req.Tools))
	for _, definition := range req.Tools {
		names = append(names, definition.Function.Name)
	}
	_ = recorder.RecordRequest(tracepkg.Event{Model: model,
		Metadata: map[string]any{"visible_tools": names, "constrained": constrained, "format": string(req.Format), "options": req.Options}},
		req.Messages, req.Tools)
}

func filterTools(all []tools.Tool, f func(string) bool) []tools.Tool {
	if f == nil {
		return all
	}
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		if f(t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

// observeRound feeds one finished tool round through the loop guards. It
// returns stop when a detector crossed its hard threshold — the caller then
// forces the final synthesis — and otherwise the warnings to show the model.
func observeRound(g *LoopGuard, calls []tools.ToolCall, results []api.Message) (stop bool, advisories []string) {
	progress, warnOscillation, stopOscillation := g.ObserveRoundProgress(calls, results)
	warnStagnant, stopStagnant := g.ObserveStagnation(progress)
	tool, warnRepeat, stopRepeat, _ := g.ObserveRepeatedBatch(calls, progress)
	rereads, stopRereads := g.ObserveFileReads(calls)
	if stopOscillation || stopStagnant || stopRepeat || stopRereads {
		return true, nil
	}
	if warnOscillation && !g.OscillationWarned {
		g.OscillationWarned = true
		advisories = append(advisories, AdvisoryOscillation)
	}
	if warnStagnant {
		advisories = append(advisories, AdvisoryStagnation)
	} else if warnRepeat {
		// Stagnation is the stronger statement about the same round; only one speaks.
		advisories = append(advisories, AdvisoryRepeat(tool, g.SameToolStreak))
	}
	if len(rereads) > 0 {
		advisories = append(advisories, AdvisoryReread(rereads))
	}
	return false, advisories
}

// VerificationFailed is the message that hands a failed check back to the
// model. Shared with the interactive gate so both loops say the same thing.
func VerificationFailed(command, output string) string {
	return fmt.Sprintf("[VERIFICATION FAILED] You are NOT done — `%s` failed. Read the errors, fix the actual cause (don't blame the tools, and don't weaken or delete tests), then it will be re-checked:\n\n%s", command, output)
}

// runVerification runs the project's check over the files this run changed.
// ran is false when there is nothing to check: Verify off, no changes, or no
// plan detectable for this project.
func runVerification(ctx context.Context, opts Options, changed map[string]bool) (command, output string, passed, ran bool) {
	if !opts.Verify || len(changed) == 0 {
		return "", "", false, false
	}
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	plan, ok := verification.Detect(".", paths, opts.VerifyCmd)
	if !ok {
		return "", "", false, false
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := tools.NewShellCommand(cctx, plan.Command).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 4000 {
		text = "…\n" + text[len(text)-4000:] // compiler and test errors cluster at the end
	}
	if opts.Trace != nil {
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		_ = opts.Trace.Record(tracepkg.Event{Kind: "verification", Model: opts.Model, Result: text, Error: errText,
			Metadata: map[string]any{"command": plan.Command, "label": plan.Label, "ok": err == nil}})
	}
	return plan.Command, text, err == nil, true
}

// verifyGate runs when the model tries to finish. On a failing check, while
// budget remains, it returns the feedback to hand back and retry=true.
func verifyGate(ctx context.Context, opts Options, changed map[string]bool, res *Result) (feedback string, retry bool) {
	if res.Verification == "passed" {
		return "", false
	}
	command, output, passed, ran := runVerification(ctx, opts, changed)
	switch {
	case !ran:
		return "", false
	case passed:
		res.Verification = "passed"
		return "", false
	case res.VerifyFailures >= maxVerifyAttempts:
		res.Verification = "failed"
		return "", false
	}
	res.VerifyFailures++
	return VerificationFailed(command, output), true
}

// verifyBeforeReport checks the changed files once more when a guard or the
// step limit ended the run, so the forced report cannot claim success on code
// that fails: the verdict goes into the report prompt. No retry — the run is over.
func verifyBeforeReport(ctx context.Context, opts Options, changed map[string]bool, res *Result) string {
	if res.Verification == "passed" {
		return ""
	}
	command, output, passed, ran := runVerification(ctx, opts, changed)
	if !ran {
		return ""
	}
	if passed {
		res.Verification = "passed"
		return fmt.Sprintf("Verification (`%s`) passes on the files you changed.", command)
	}
	res.Verification = "failed"
	return fmt.Sprintf("Verification (`%s`) FAILS on the files you changed — the task is NOT complete. Say so plainly in your report and include what is still failing:\n\n%s", command, output)
}
