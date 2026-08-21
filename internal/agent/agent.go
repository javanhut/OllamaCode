// Package agent provides a minimal, non-streaming agent loop reused by the
// in-session sub-agent tool and the eval harness. It deliberately mirrors the
// TUI loop's safety posture (step cap, content-parse fallback, tool filtering)
// but without any UI or streaming.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
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
	recentCallsKept   = 8 // fingerprint ring length for oscillation detection
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
	var recent []string         // ring of recent fingerprints for oscillation

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
			res.Output = output
			res.Messages = append(msgs, api.Message{Role: "assistant", Content: output})
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

		progressed := false
		for _, c := range calls {
			res.ToolsUsed = append(res.ToolsUsed, c.Function.Name)
			if opts.ToolFilter != nil && !opts.ToolFilter(c.Function.Name) {
				res.ToolErrors++
				msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name,
					Content: "error: tool not permitted for this agent"})
				continue
			}
			fp := tools.CallFingerprint(c)
			recent = append(recent, fp)
			if len(recent) > recentCallsKept {
				recent = recent[1:]
			}
			// Stuck-guard: refuse an identical call the model keeps re-issuing
			// rather than re-running it, so a weak model can't burn the budget
			// looping on one action.
			if fpCount[fp] >= maxIdenticalCalls {
				res.RepeatedBlocked++
				msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name,
					Content: fmt.Sprintf("error: you already ran this exact call %d times with the same result. Stop repeating it — use what you already have, or take a materially different action.", fpCount[fp])})
				continue
			}
			fpCount[fp]++
			progressed = true

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
			msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name, Content: event.Result})
		}

		// No forward motion — every call this round was a refused repeat, or the
		// model is oscillating A/B/A/B. Stop and synthesize what we have.
		if !progressed || tools.IsOscillating(recent) {
			break
		}
	}

	// Didn't answer on its own: force one tool-less pass so partial findings come
	// back instead of a useless "hit the limit" sentinel.
	res.HitLimit = true
	output, history, promptTokens, completionTokens := finalize(ctx, host, opts, options, msgs)
	res.Output = output
	res.Messages = history
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
func finalize(ctx context.Context, host ChatClient, opts Options, options map[string]any, msgs []api.Message) (string, []api.Message, int, int) {
	// Advisory: the harness wrote this, not the user. Without the flag the trace
	// exporter reads it as the sub-agent's task and every limit-hitting
	// trajectory trains on the nudge instead of the real prompt.
	msgs = append(msgs, api.Message{Role: "user", Advisory: true, Content: "Stop. Do NOT call any more tools. Based on everything above, write your final report now: a direct answer to the task plus the concrete file paths, line references, and commands you used. If you couldn't finish, say what you found and what remains."})
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
