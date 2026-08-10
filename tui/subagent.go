package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
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

// subagentCallIsAsync reports whether a spawn_subagent call runs in the
// background. Async is the default; only an explicit "async": false blocks.
func subagentCallIsAsync(call tools.ToolCall) bool {
	if call.Function.Name != "spawn_subagent" {
		return false
	}
	var a struct {
		Async *bool `json:"async"`
	}
	_ = json.Unmarshal(call.Function.Arguments, &a)
	return a.Async == nil || *a.Async
}

// spawnSubagentTool delegates one or more self-contained tasks to autonomous
// sub-agents. By default the call returns immediately with a job handle and the
// sub-agents run in the background; their reports arrive as a completion
// notification injected into the conversation (see subagent_bg.go). With
// async=false a single task runs inline and multiple tasks fan out in parallel
// (bounded), blocking until every report is in. Sub-agents inherit the parent's
// safety mode: read-only in explore/plan, full capability in write/auto.
func (m *Model) spawnSubagentTool() tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name: "spawn_subagent",
			Description: "Delegate self-contained task(s) to autonomous sub-agents. By default (async=true) the sub-agents run in the BACKGROUND: this call returns immediately with a job id, and you are notified with their full reports when they finish — keep working on other things in the meantime, and do not repeat the spawn while you wait. Pass async=false to block until every sub-agent reports back inline. Each sub-agent has its own bounded loop and full capability within your current mode (always read/search; in write mode also edit/write files and run shell). Pass MULTIPLE tasks to run them in PARALLEL — ideal for independent work, e.g. investigate three modules at once, or apply an unrelated change in each of several files. WARNING: parallel sub-agents run concurrently with NO cross-task conflict detection — only parallelize tasks that touch INDEPENDENT files. Sub-agent file edits are checkpointed for /undo, so one /undo rewinds a delegation (they are not individually undoable); a background sub-agent that outlives this turn banks its edits into whichever turn checkpoint is open when the write happens. Give each task enough context to work without seeing this conversation.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"task":  {Type: "string", Description: "A single self-contained task. Use this OR tasks."},
					"tasks": {Type: "array", Description: "Multiple self-contained tasks to run in parallel. Use for independent work on non-overlapping files.", Items: &tools.Property{Type: "string"}},
					"async": {Type: "boolean", Description: "Run in the background and notify on completion (default true). Pass false to block until the sub-agents report back inline."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Task  string   `json:"task"`
				Tasks []string `json:"tasks"`
				Async *bool    `json:"async"`
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

			if a.Async == nil || *a.Async {
				return m.spawnSubagentAsync(tasks)
			}

			// Sync path: block until every child has reported. The executor runs
			// Before synchronously right before each tool call and this handler
			// only returns after every child has finished, so all snapshots land
			// while the parent turn is still open — one /undo rewinds the whole
			// delegation.
			opts := m.subagentOptions(m.checkpointBeforeCall(), nil)
			return m.runSubagentTasks(ctx, tasks, opts)
		},
	}
}

// subagentOptions builds the headless-run options shared by the sync and
// background paths. The mode is snapshotted once so parallel workers don't
// race on m.mode. before banks child file mutations into the parent's /undo
// checkpoint (see checkpointBeforeCall); onMutation, when set, is notified of
// each file-mutating call so a background job can record that it wrote files.
func (m *Model) subagentOptions(before func(tools.ToolCall), onMutation func()) agent.Options {
	mode := m.mode
	constrain, constraintCache := m.subagentConstraintOptions()
	if onMutation != nil {
		base := before
		before = func(call tools.ToolCall) {
			if len(tools.MutatedPaths(call.Function.Name, call.Function.Arguments)) > 0 {
				onMutation()
			}
			if base != nil {
				base(call)
			}
		}
	}
	return agent.Options{
		Model:    m.modelName,
		System:   subagentSystem,
		MaxSteps: subagentMaxSteps,
		NumCtx:   m.contextLimit,
		ToolFilter: func(name string) bool {
			return !subagentExcluded[name] && toolAllowedInMode(mode, name)
		},
		ConstrainToolCalls: constrain,
		Constraints:        constraintCache,
		Before:             before,
	}
}

// runAgentTask runs one headless sub-agent. Tests substitute m.agentRunner to
// avoid a live model; nil means the real loop against the configured host.
func (m *Model) runAgentTask(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
	if m.agentRunner != nil {
		return m.agentRunner(ctx, task, opts)
	}
	return agent.Run(ctx, m.host, m.tools, task, opts)
}

// runSubagentTasks executes the tasks: a single task runs inline, multiple
// tasks fan out in parallel (bounded). One task's failure doesn't cancel its
// siblings. Returns the combined report.
func (m *Model) runSubagentTasks(ctx context.Context, tasks []string, opts agent.Options) (string, error) {
	if len(tasks) == 1 {
		res, err := m.runAgentTask(ctx, tasks[0], opts)
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
			res, err := m.runAgentTask(gctx, task, opts)
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
}
