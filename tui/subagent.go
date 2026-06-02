package tui

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/mcp"
)

// subagentExcluded are read-only tools a sub-agent must NOT use: no recursion,
// no mode switching, no user prompts, no session/memory mutation. A sub-agent is
// a pure, sandboxed investigator.
var subagentExcluded = map[string]bool{
	"spawn_subagent":       true,
	"switch_mode":          true,
	"ask_user":             true,
	"remember":             true,
	"recall":               true,
	"forget":               true,
	"update_session_notes": true,
	"append_session_notes": true,
}

// subagentAllowed reports whether a tool is permitted inside a sub-agent run.
func subagentAllowed(name string) bool {
	return readOnlyToolNames[name] && !subagentExcluded[name]
}

const subagentSystem = `You are a focused, READ-ONLY sub-agent spawned to investigate one task and report back. You cannot modify files or run mutating commands — only read, search, and analyze. Be efficient: gather what you need, then return a concise, concrete findings report (file paths, line references, and a direct answer). Do not ask questions; if something is ambiguous, state your assumption and proceed.`

// spawnSubagentTool delegates a self-contained, read-only investigation to a
// bounded child agent and returns its findings. It's safe to run concurrently
// (multiple sub-agents in one tool batch) because it only reads, never mutates
// Model state or the filesystem.
func (m *Model) spawnSubagentTool() mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "spawn_subagent",
			Description: "Delegate a focused, READ-ONLY investigation to a sub-agent and get back its findings. Ideal for parallelizable research that would otherwise clutter your own context — e.g. \"find every caller of function X and summarize how it's used\" or \"explain how the auth flow works across these files\". The sub-agent can read, search, and analyze but CANNOT modify files or run mutating commands. Spawn several at once for independent investigations.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"task": {Type: "string", Description: "A self-contained investigation task. Include enough context for the sub-agent to work without seeing this conversation."},
				},
				Required: []string{"task"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Task string `json:"task"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Task == "" {
				return "", fmt.Errorf("task is required")
			}
			res, err := agent.Run(ctx, m.host, m.tools, a.Task, agent.Options{
				Model:      m.modelName,
				System:     subagentSystem,
				MaxSteps:   8,
				NumCtx:     m.contextLimit,
				ToolFilter: subagentAllowed,
			})
			if err != nil {
				return "", fmt.Errorf("sub-agent failed: %w", err)
			}
			return res.Output, nil
		},
	}
}
