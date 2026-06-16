package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/mcp"
)

// parallel_edit splits one large change into independent subtasks, plans each
// with a read-only worker IN PARALLEL, then applies the collected edits SERIALLY
// through the normal write path. This is the "parallel plan, serial apply"
// model: the expensive reasoning fans out, but no two writes ever race because
// nothing touches disk until every worker has finished and the orchestrator
// replays the staged edits one at a time (with /undo checkpoints and conflict
// detection inherited from the real edit tools).
const (
	maxParallelEditTasks   = 8
	maxParallelEditWorkers = 4
	parallelEditMaxSteps   = 12
)

const plannerSystem = `You are a focused worker assigned ONE slice of a larger change. First investigate read-only (read_file, grep, find_symbol, etc.) to understand exactly what must change. Then propose your edits by calling stage_edit / stage_write / stage_delete — these DO NOT modify files now; they queue your changes, which the orchestrator applies in order through the safe write path after every worker finishes. You CANNOT run shell commands or write files directly. Stay strictly within your assigned scope; never stage changes to a file another worker owns. stage_edit needs the exact text currently in the file. When you have staged every change your task needs, reply with a single concise line summarizing what you proposed.`

// stagedOp is one proposed file mutation captured by a worker's staging tools.
type stagedOp struct {
	kind       string // "edit" | "write" | "delete"
	path       string
	oldString  string
	newString  string
	replaceAll bool
	content    string
	recursive  bool
	summary    string
}

// editStage collects a single worker's proposed ops. Guarded by a mutex because
// the worker's tool calls, while normally sequential, share it with the loop.
type editStage struct {
	mu  sync.Mutex
	ops []stagedOp
}

func (s *editStage) add(op stagedOp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *editStage) list() []stagedOp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stagedOp(nil), s.ops...)
}

// plannerToolNames are the staging tools a worker uses in place of real writes.
var plannerToolNames = map[string]bool{
	"stage_edit":   true,
	"stage_write":  true,
	"stage_delete": true,
}

// plannerAllowed is the tool gate for a planning worker: the read-only
// sub-agent set (minus code_index, which writes an index file and would race
// across parallel workers) plus the staging tools.
func plannerAllowed(name string) bool {
	if plannerToolNames[name] {
		return true
	}
	return subagentAllowed(name) && name != "code_index"
}

// plannerRegistry builds an isolated tool registry for one worker: the allowed
// read-only tools plus staging tools bound to that worker's own stage, so
// concurrent workers never share an edit buffer.
func (m *Model) plannerRegistry(stage *editStage) *mcp.Registry {
	reg := mcp.NewRegistry()
	for _, t := range m.tools.Definitions() {
		if plannerAllowed(t.Function.Name) {
			reg.Register(t)
		}
	}
	reg.Register(stageEditTool(stage))
	reg.Register(stageWriteTool(stage))
	reg.Register(stageDeleteTool(stage))
	return reg
}

func stageEditTool(stage *editStage) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "stage_edit",
			Description: "Propose an exact-string replacement in a file. Does NOT write now — the edit is queued and applied later by the orchestrator. old_string must already exist in the file (and be unique unless replace_all is set), mirroring edit_file.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"path":        {Type: "string", Description: "File to edit."},
					"old_string":  {Type: "string", Description: "Exact text currently in the file to replace."},
					"new_string":  {Type: "string", Description: "Replacement text."},
					"replace_all": {Type: "boolean", Description: "Replace every occurrence (default false)."},
					"summary":     {Type: "string", Description: "One-line description of this change."},
				},
				Required: []string{"path", "old_string", "new_string"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
				Summary    string `json:"summary"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.OldString == "" {
				return "", fmt.Errorf("path and old_string are required")
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", fmt.Errorf("cannot read %s: %w", a.Path, err)
			}
			n := strings.Count(string(data), a.OldString)
			if n == 0 {
				return "", fmt.Errorf("old_string not found in %s — read the file and copy the exact text", a.Path)
			}
			if n > 1 && !a.ReplaceAll {
				return "", fmt.Errorf("old_string appears %d times in %s — make it unique or set replace_all=true", n, a.Path)
			}
			stage.add(stagedOp{kind: "edit", path: a.Path, oldString: a.OldString, newString: a.NewString, replaceAll: a.ReplaceAll, summary: a.Summary})
			return fmt.Sprintf("staged edit to %s (applied after all workers finish)", a.Path), nil
		},
	}
}

func stageWriteTool(stage *editStage) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "stage_write",
			Description: "Propose writing a file's full contents (creating it or overwriting it). Does NOT write now — queued and applied later by the orchestrator. Prefer stage_edit for incremental changes to existing files.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"path":    {Type: "string", Description: "File to create or overwrite."},
					"content": {Type: "string", Description: "Full new contents of the file."},
					"summary": {Type: "string", Description: "One-line description of this change."},
				},
				Required: []string{"path", "content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			stage.add(stagedOp{kind: "write", path: a.Path, content: a.Content, summary: a.Summary})
			return fmt.Sprintf("staged write to %s (applied after all workers finish)", a.Path), nil
		},
	}
}

func stageDeleteTool(stage *editStage) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "stage_delete",
			Description: "Propose deleting a file (or directory tree with recursive=true). Does NOT delete now — queued and applied later by the orchestrator.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"path":      {Type: "string", Description: "File or directory to delete."},
					"recursive": {Type: "boolean", Description: "Delete a directory and its contents (default false)."},
					"summary":   {Type: "string", Description: "One-line description of this change."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				Recursive bool   `json:"recursive"`
				Summary   string `json:"summary"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			stage.add(stagedOp{kind: "delete", path: a.Path, recursive: a.Recursive, summary: a.Summary})
			return fmt.Sprintf("staged delete of %s (applied after all workers finish)", a.Path), nil
		},
	}
}

// applyStagedOp replays one staged op through the real write tool so it inherits
// validation, /undo checkpointing, and the file-change hook. Called serially.
func (m *Model) applyStagedOp(ctx context.Context, op stagedOp) (string, error) {
	var name string
	var payload map[string]any
	switch op.kind {
	case "edit":
		name = "edit_file"
		payload = map[string]any{"path": op.path, "old_string": op.oldString, "new_string": op.newString, "replace_all": op.replaceAll}
	case "write":
		name = "write_file"
		payload = map[string]any{"path": op.path, "content": op.content}
	case "delete":
		name = "delete_file"
		payload = map[string]any{"path": op.path, "recursive": op.recursive}
	default:
		return "", fmt.Errorf("unknown op kind %q", op.kind)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	m.snapshotBeforeMutate([]string{op.path})
	return m.tools.Invoke(ctx, mcp.ToolCall{Function: mcp.ToolCallFunction{Name: name, Arguments: raw}})
}

// parallelEditTool is the orchestrator: fan out planning workers, then apply
// their proposals serially and safely. Registered as a destructive tool so the
// normal permission prompt gates the whole delegation once before it runs.
func (m *Model) parallelEditTool() mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "parallel_edit",
			Description: "Split a large change into independent subtasks and complete them faster than one agent could. Each subtask is planned by its own read-only worker IN PARALLEL; the workers propose edits, which are then applied SERIALLY and safely (with /undo checkpoints and conflict detection). Give each subtask a DISJOINT set of files — workers must not edit the same file. Overlapping or stale edits are reported as conflicts, never silently merged. Use for genuinely parallelizable work (e.g. \"rename X across these 5 packages\", \"add the same guard to each of these handlers\"). For a single focused change, just edit directly.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"tasks": {
						Type:        "array",
						Description: "Independent subtasks. Each must be self-contained (the worker does not see this conversation) and target files disjoint from the others.",
						Items: &mcp.Property{
							Type: "object",
							Properties: map[string]mcp.Property{
								"task":  {Type: "string", Description: "Self-contained instruction for this slice of the change."},
								"files": {Type: "array", Description: "Files this subtask owns (advisory scope hint).", Items: &mcp.Property{Type: "string"}},
							},
							Required: []string{"task"},
						},
					},
				},
				Required: []string{"tasks"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Tasks []struct {
					Task  string   `json:"task"`
					Files []string `json:"files,omitempty"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if len(a.Tasks) == 0 {
				return "", fmt.Errorf("tasks is required: a list of independent change subtasks")
			}
			if len(a.Tasks) > maxParallelEditTasks {
				return "", fmt.Errorf("too many tasks (%d); max %d — group them into fewer subtasks", len(a.Tasks), maxParallelEditTasks)
			}
			for i, t := range a.Tasks {
				if strings.TrimSpace(t.Task) == "" {
					return "", fmt.Errorf("task %d is empty", i+1)
				}
			}

			type workerResult struct {
				task   string
				stage  *editStage
				output string
				err    error
			}
			results := make([]workerResult, len(a.Tasks))
			sem := make(chan struct{}, maxParallelEditWorkers)
			var wg sync.WaitGroup
			for i := range a.Tasks {
				stage := &editStage{}
				results[i] = workerResult{task: a.Tasks[i].Task, stage: stage}
				wg.Add(1)
				go func(i int, task string, files []string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					prompt := task
					if len(files) > 0 {
						prompt += "\n\nFiles in your scope (do not modify any others): " + strings.Join(files, ", ")
					}
					res, err := agent.Run(ctx, m.host, m.plannerRegistry(stage), prompt, agent.Options{
						Model:    m.modelName,
						System:   plannerSystem,
						MaxSteps: parallelEditMaxSteps,
						NumCtx:   m.contextLimit,
					})
					results[i].output = res.Output
					results[i].err = err
				}(i, a.Tasks[i].Task, a.Tasks[i].Files)
			}
			wg.Wait()

			// Record intended file ownership across workers so cross-worker
			// overlaps can be flagged before they collide at apply time.
			owner := map[string]int{}
			for i := range results {
				for _, op := range results[i].stage.list() {
					if _, ok := owner[op.path]; !ok {
						owner[op.path] = i
					}
				}
			}

			// Serial apply — no two writes ever run concurrently.
			var b strings.Builder
			applied, conflicts, skipped := 0, 0, 0
			for i := range results {
				r := results[i]
				fmt.Fprintf(&b, "\n[worker %d] %s\n", i+1, peClip(r.task, 100))
				if r.err != nil {
					fmt.Fprintf(&b, "  planning failed: %v\n", r.err)
					continue
				}
				ops := r.stage.list()
				if len(ops) == 0 {
					fmt.Fprintf(&b, "  proposed no changes — %s\n", peClip(r.output, 160))
					continue
				}
				for _, op := range ops {
					if !m.isPathInTrustedFolder(op.path) {
						skipped++
						fmt.Fprintf(&b, "  SKIP %s — outside workspace, apply it manually\n", op.path)
						continue
					}
					if own, ok := owner[op.path]; ok && own != i {
						fmt.Fprintf(&b, "  NOTE %s is also owned by worker %d (overlap)\n", op.path, own+1)
					}
					out, err := m.applyStagedOp(ctx, op)
					if err != nil {
						conflicts++
						fmt.Fprintf(&b, "  CONFLICT %s — %s\n", op.path, peFirstLine(err.Error()))
						continue
					}
					applied++
					label := op.summary
					if label == "" {
						label = peFirstLine(out)
					}
					fmt.Fprintf(&b, "  applied %s — %s\n", op.path, peClip(label, 100))
				}
			}

			header := fmt.Sprintf("parallel_edit: %d worker(s) · %d change(s) applied · %d conflict(s) · %d skipped.", len(results), applied, conflicts, skipped)
			if conflicts > 0 {
				header += " Resolve conflicts by editing the affected files directly, or re-run with non-overlapping scopes."
			}
			return header + b.String(), nil
		},
	}
}

func peFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func peClip(s string, n int) string {
	s = strings.TrimSpace(peFirstLine(s))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
