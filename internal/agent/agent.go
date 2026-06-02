// Package agent provides a minimal, non-streaming agent loop reused by the
// in-session sub-agent tool and the eval harness. It deliberately mirrors the
// TUI loop's safety posture (step cap, content-parse fallback, tool filtering)
// but without any UI or streaming.
package agent

import (
	"context"

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

// Run executes a bounded agent loop: prompt the model, dispatch any tool calls
// (native or parsed from content), feed results back, repeat until the model
// answers without calling tools or the step budget is exhausted.
func Run(ctx context.Context, host ChatClient, reg *mcp.Registry, task string, opts Options) (Result, error) {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 8
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

		res.Steps++
		msgs = append(msgs, api.Message{Role: "assistant", Content: resp.Message.Content, ToolCalls: calls})
		for _, c := range calls {
			res.ToolsUsed = append(res.ToolsUsed, c.Function.Name)
			if opts.ToolFilter != nil && !opts.ToolFilter(c.Function.Name) {
				msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name,
					Content: "error: tool not permitted for this agent"})
				continue
			}
			out, err := reg.Invoke(ctx, c)
			if err != nil {
				out = "error: " + err.Error()
			}
			msgs = append(msgs, api.Message{Role: "tool", ToolName: c.Function.Name, Content: out})
		}
	}
	res.HitLimit = true
	res.Output = "(sub-agent reached its step limit without a final answer)"
	return res, nil
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
