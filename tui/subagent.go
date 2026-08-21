package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/internal/jobs"
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
			Name:        "spawn_subagent",
			Description: "Delegate self-contained task(s) to autonomous sub-agents. By default (async=true) the sub-agents run in the BACKGROUND: this call returns immediately with a job id, and you are notified with their full reports when they finish — keep working on other things in the meantime, and do not repeat the spawn while you wait. Pass async=false to block until every sub-agent reports back inline. Each sub-agent has its own bounded loop and full capability within your current mode (always read/search; in write mode also edit/write files and run shell). Pass MULTIPLE tasks to run them in PARALLEL — ideal for independent work, e.g. investigate three modules at once, or apply an unrelated change in each of several files. WARNING: parallel sub-agents run concurrently with NO cross-task conflict detection — only parallelize tasks that touch INDEPENDENT files. Sub-agent file edits are checkpointed for /undo, so one /undo rewinds a delegation (they are not individually undoable); a background sub-agent that outlives this turn banks its edits into whichever turn checkpoint is open when the write happens. Give each task enough context to work without seeing this conversation. To ask a FINISHED background sub-agent a follow-up, pass resume_job with the job id from its completion notice and put the follow-up in task — the child resumes with its full prior conversation and a fresh step budget, which is cheaper and better-informed than spawning a new agent.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"task":       {Type: "string", Description: "A single self-contained task — or, with resume_job, the follow-up message for the finished sub-agent. Use this OR tasks."},
					"tasks":      {Type: "array", Description: "Multiple self-contained tasks to run in parallel. Use for independent work on non-overlapping files.", Items: &tools.Property{Type: "string"}},
					"async":      {Type: "boolean", Description: "Run in the background and notify on completion (default true). Pass false to block until the sub-agents report back inline."},
					"resume_job": {Type: "integer", Description: "Resume a finished background sub-agent: the job id from its completion notice. task becomes the follow-up message and the child continues with its retained conversation. Works for completed, failed, timed-out, and cancelled jobs. Cannot be combined with tasks."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Task      string   `json:"task"`
				Tasks     []string `json:"tasks"`
				Async     *bool    `json:"async"`
				ResumeJob *int     `json:"resume_job"`
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

			if a.ResumeJob != nil {
				return m.resumeSubagent(ctx, *a.ResumeJob, tasks, a.Async)
			}

			if len(tasks) == 0 {
				return "", fmt.Errorf("task (or tasks) is required")
			}

			if a.Async == nil || *a.Async {
				return m.spawnSubagentAsync(tasks, nil)
			}

			// Sync path: block until every child has reported. The executor runs
			// Before synchronously right before each tool call and this handler
			// only returns after every child has finished, so all snapshots land
			// while the parent turn is still open — one /undo rewinds the whole
			// delegation.
			opts := m.subagentOptions(m.checkpointBeforeCall(), nil)
			report, _, err := m.runSubagentTasks(ctx, tasks, opts)
			return report, err
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
		Permissions:        m.cfg.Permissions,
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
// siblings. Returns the combined report and, for a single task, the child's
// full conversation (retained for later follow-ups; nil for parallel fan-outs
// — per-child histories of a multi-task job are not kept). The history is
// returned even on error, holding the exchanges up to the failure.
func (m *Model) runSubagentTasks(ctx context.Context, tasks []string, opts agent.Options) (string, []api.Message, error) {
	if len(tasks) == 1 {
		res, err := m.runAgentTask(ctx, tasks[0], opts)
		if err != nil {
			return "", res.Messages, fmt.Errorf("sub-agent failed: %w", err)
		}
		return res.Output, res.Messages, nil
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
	return strings.TrimRight(b.String(), "\n"), nil, nil
}

// resumeSubagent continues a finished background sub-agent with a follow-up
// message: the child runs again seeded with its retained conversation plus
// the follow-up, on a fresh step budget with the same system prompt and tool
// filter as the original run. Async (the default) registers a NEW job linked
// to the original in its label and notifies on completion; async=false blocks
// and returns the report inline, updating the original job's retained history
// so further follow-ups keep referencing the same job id.
func (m *Model) resumeSubagent(ctx context.Context, jobID int, tasks []string, async *bool) (string, error) {
	if len(tasks) != 1 {
		return "", fmt.Errorf("resume_job takes exactly one follow-up message in task (got %d) — it cannot be combined with tasks", len(tasks))
	}
	job, history, err := m.resumeTarget(jobID)
	if err != nil {
		return "", err
	}
	followup := tasks[0]

	if async == nil || *async {
		return m.spawnSubagentAsync([]string{followup}, &subagentResume{jobID: jobID, history: history})
	}

	opts := m.subagentOptions(m.checkpointBeforeCall(), nil)
	opts.PriorMessages = history
	report, newHistory, runErr := m.runSubagentTasks(ctx, []string{followup}, opts)
	if len(newHistory) > 0 {
		m.subagents.retain(job, newHistory)
	}
	if runErr != nil {
		return "", fmt.Errorf("sub-agent follow-up failed: %w", runErr)
	}
	return report, nil
}

// resumeTarget validates a resume_job reference and returns the finished job
// and its retained conversation. The errors are model-facing: the id must
// name a known sub-agent job (not a shell job, not unknown) that has finished
// (a running job must be awaited first) and whose history is still retained
// (killed, failed, and timed-out jobs keep theirs).
func (m *Model) resumeTarget(jobID int) (*subagentJob, []api.Message, error) {
	m.ensureSubagentRuntime()
	m.subagents.mu.Lock()
	job, ok := m.subagents.jobs[jobID]
	m.subagents.mu.Unlock()
	if !ok {
		if rec, found := jobs.Default().Get(jobID); found {
			return nil, nil, fmt.Errorf("job %d is a %s job, not a sub-agent job — only sub-agent jobs can be resumed", jobID, rec.Kind())
		}
		return nil, nil, fmt.Errorf("no sub-agent job %d — pass the job id from a sub-agent completion notice", jobID)
	}
	if done, _, _ := job.snapshot(); !done {
		return nil, nil, fmt.Errorf("sub-agent job %d is still running — wait for its completion notice, then send the follow-up", jobID)
	}
	history := job.historySnapshot()
	if len(history) == 0 {
		return nil, nil, fmt.Errorf("sub-agent job %d has no retained conversation (it ran multiple tasks in parallel, or its history was evicted — only the last %d finished sub-agents are kept) — spawn a fresh sub-agent instead", jobID, maxRetainedSubagentHistories)
	}
	return job, history, nil
}
