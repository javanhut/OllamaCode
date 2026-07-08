package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/mcp"
)

// maxParallelSubagents bounds how many sub-agents run concurrently for one
// spawn_subagent call, so a model can't fork an unbounded fleet.
const maxParallelSubagents = 4

// subagentMaxSteps is the per-sub-agent tool-round budget. Generous because the
// loop now detects no-progress and finalizes early, so the headroom isn't wasted
// on a stuck model.
const subagentMaxSteps = 20

// subagentExcluded are tools that structurally cannot work in a headless child:
// recursion, mode switching, and user prompts. Everything else — including
// write_file, edit_file, delete_file, and run_shell — is permitted, subject to
// the parent's current safety mode (see the filter in spawnSubagentTool).
var subagentExcluded = map[string]bool{
	"spawn_subagent": true, // no recursion — a sub-agent can't spawn sub-agents
	"switch_mode":    true, // no mode concept inside a headless child
	"ask_user":       true, // headless: there's no user to prompt
}

const subagentSystem = `You are an autonomous sub-agent spawned to complete ONE self-contained task end to end, then report back. You have full capability within the current safety mode: read and search the codebase, edit and write files, and run shell commands. Work decisively — gather the context you need, make the change or find the answer, verify it, and stop. Return a concise, concrete report: what you did or found, with exact file paths, line references, and any commands you ran. Do NOT ask questions; if something is ambiguous, state your assumption and proceed. When the task is complete, reply WITHOUT calling any tools.`

// spawnSubagentTool delegates one or more self-contained tasks to autonomous
// sub-agents. A single task runs inline; multiple tasks fan out in parallel
// (bounded). Sub-agents inherit the parent's safety mode: read-only in
// explore/plan, full capability in write/auto.
func (m *Model) spawnSubagentTool() mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "spawn_subagent",
			Description: "Delegate self-contained task(s) to autonomous sub-agents that run to completion and report back. Each sub-agent has its own bounded loop and full capability within your current mode (always read/search; in write mode also edit/write files and run shell). Pass MULTIPLE tasks to run them in PARALLEL — ideal for independent work, e.g. investigate three modules at once, or apply an unrelated change in each of several files. WARNING: parallel sub-agents run concurrently with NO cross-task conflict detection, and their file edits are not individually checkpointed for /undo — only parallelize tasks that touch INDEPENDENT files. Give each task enough context to work without seeing this conversation.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"task":  {Type: "string", Description: "A single self-contained task. Use this OR tasks."},
					"tasks": {Type: "array", Description: "Multiple self-contained tasks to run in parallel. Use for independent work on non-overlapping files.", Items: &mcp.Property{Type: "string"}},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Task  string   `json:"task"`
				Tasks []string `json:"tasks"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			tasks := make([]string, 0, len(a.Tasks)+1)
			for _, t := range append(a.Tasks, a.Task) {
				if strings.TrimSpace(t) != "" {
					tasks = append(tasks, t)
				}
			}
			if len(tasks) == 0 {
				return "", fmt.Errorf("task (or tasks) is required")
			}

			// Snapshot the mode once so parallel workers don't race on m.mode.
			mode := m.mode
			opts := agent.Options{
				Model:    m.modelName,
				System:   subagentSystem,
				MaxSteps: subagentMaxSteps,
				NumCtx:   m.contextLimit,
				ToolFilter: func(name string) bool {
					return !subagentExcluded[name] && toolAllowedInMode(mode, name)
				},
			}

			if len(tasks) == 1 {
				res, err := agent.Run(ctx, m.host, m.tools, tasks[0], opts)
				if err != nil {
					return "", fmt.Errorf("sub-agent failed: %w", err)
				}
				return res.Output, nil
			}

			// Parallel fan-out, bounded. Each result lands in its own slot (no
			// locking needed); one task's failure doesn't cancel its siblings.
			results := make([]string, len(tasks))
			g, gctx := errgroup.WithContext(ctx)
			g.SetLimit(maxParallelSubagents)
			for i, task := range tasks {
				g.Go(func() error {
					res, err := agent.Run(gctx, m.host, m.tools, task, opts)
					if err != nil {
						results[i] = fmt.Sprintf("(failed: %v)", err)
						return nil
					}
					results[i] = res.Output
					return nil
				})
			}
			_ = g.Wait()

			var b strings.Builder
			for i, out := range results {
				fmt.Fprintf(&b, "### Sub-agent %d — %s\n%s\n\n", i+1, truncatePlain(tasks[i], 80), out)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}
