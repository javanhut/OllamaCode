// Package agent provides a minimal, non-streaming agent loop reused by the
// in-session sub-agent tool and the eval harness. It deliberately mirrors the
// TUI loop's safety posture (step cap, content-parse fallback, tool filtering)
// but without any UI or streaming.
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

// ChatClient is the subset of api.OllamaHost the loop needs; an interface so the
// loop can be unit-tested with a fake. api.OllamaHost satisfies it.
type ChatClient interface {
	ChatOnce(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error)
}

// Options configures a headless run.
type Options struct {
	Model      string
	System     string
	MaxSteps   int                    // tool-call rounds before giving up (default 8)
	NumCtx     int                    // num_ctx option, if > 0
	ToolFilter func(name string) bool // which tools the agent may see/call (nil = all)
}

// Result is the outcome of a headless run.
type Result struct {
	Output    string // the model's final (non-tool) message
	Steps     int    // tool-call rounds executed
	HitLimit  bool   // true if MaxSteps was reached without a final answer
	ToolsUsed []string
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
func Run(ctx context.Context, host ChatClient, reg *mcp.Registry, task string, opts Options) (Result, error) {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxSteps
	}
	tools := filterTools(reg.Definitions(), opts.ToolFilter)
	options := map[string]any{}
	if opts.NumCtx > 0 {
		options["num_ctx"] = opts.NumCtx
	}

	var msgs []api.Message
	if opts.System != "" {
		msgs = append(msgs, api.Message{Role: "system", Content: opts.System})
	}
	msgs = append(msgs, api.Message{Role: "user", Content: task})

	var res Result
	fpCount := map[string]int{} // call fingerprint -> times dispatched
	var recent []string         // ring of recent fingerprints for oscillation

	for res.Steps < opts.MaxSteps {
		resp, err := host.ChatOnce(ctx, api.ChatRequest{
			Model:    opts.Model,
			Messages: msgs,
			Tools:    tools,
			Options:  options,
		})
		if err != nil {
			return res, err
		}
		calls := resp.Message.ToolCalls
		if len(calls) == 0 {
			calls = reg.ParseToolCallsFromContent(resp.Message.Content)
		}
		if len(calls) == 0 {
			res.Output = resp.Message.Content
			return res, nil
		}
		calls = mcp.DedupeCalls(calls)

		res.Steps++
		msgs = append(msgs, api.Message{Role: "assistant", Content: resp.Message.Content, ToolCalls: calls})

		progressed := false
		for _, c := range calls {
			res.ToolsUsed = append(res.ToolsUsed, c.Function.Name)
			if opts.ToolFilter != nil && !opts.ToolFilter(c.Function.Name) {
				msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name,
					Content: "error: tool not permitted for this agent"})
				continue
			}
			fp := mcp.CallFingerprint(c)
			recent = append(recent, fp)
			if len(recent) > recentCallsKept {
				recent = recent[1:]
			}
			// Stuck-guard: refuse an identical call the model keeps re-issuing
			// rather than re-running it, so a weak model can't burn the budget
			// looping on one action.
			if fpCount[fp] >= maxIdenticalCalls {
				msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name,
					Content: fmt.Sprintf("error: you already ran this exact call %d times with the same result. Stop repeating it — use what you already have, or take a materially different action.", fpCount[fp])})
				continue
			}
			fpCount[fp]++
			progressed = true

			// Same tool-call robustness as the TUI loop: salvage almost-valid JSON,
			// run with a per-call timeout + panic recovery, escalate argument
			// errors to constrained decoding, and feed back actionable hints.
			c.Function.Arguments = mcp.SalvageJSON(c.Function.Arguments)
			out, err := invokeWithTimeout(ctx, reg, c, toolTimeout(c))
			if err != nil && mcp.ShouldFormatRepair(c, err) {
				if fixed, ok := RepairArgsViaFormat(ctx, host, reg, opts.Model, opts.NumCtx, c); ok {
					c.Function.Arguments = fixed
					out, err = invokeWithTimeout(ctx, reg, c, toolTimeout(c))
				}
			}
			if err != nil {
				out = mcp.RepairHint(c, err)
			}
			msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name, Content: out})
		}

		// No forward motion — every call this round was a refused repeat, or the
		// model is oscillating A/B/A/B. Stop and synthesize what we have.
		if !progressed || mcp.IsOscillating(recent) {
			break
		}
	}

	// Didn't answer on its own: force one tool-less pass so partial findings come
	// back instead of a useless "hit the limit" sentinel.
	res.HitLimit = true
	res.Output = finalize(ctx, host, opts, options, msgs)
	return res, nil
}

// finalize asks the model, with NO tools available, to write up whatever it
// gathered. Passing no tools forces a prose answer rather than another tool call.
func finalize(ctx context.Context, host ChatClient, opts Options, options map[string]any, msgs []api.Message) string {
	msgs = append(msgs, api.Message{Role: "user", Content: "Stop. Do NOT call any more tools. Based on everything above, write your final report now: a direct answer to the task plus the concrete file paths, line references, and commands you used. If you couldn't finish, say what you found and what remains."})
	resp, err := host.ChatOnce(ctx, api.ChatRequest{
		Model:    opts.Model,
		Messages: msgs,
		Options:  options,
	})
	if err != nil || strings.TrimSpace(resp.Message.Content) == "" {
		return "(sub-agent stopped without a final answer)"
	}
	return resp.Message.Content
}

func filterTools(all []mcp.Tool, f func(string) bool) []mcp.Tool {
	if f == nil {
		return all
	}
	out := make([]mcp.Tool, 0, len(all))
	for _, t := range all {
		if f(t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}
