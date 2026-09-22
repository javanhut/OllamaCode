package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/javanhut/ollama_code/internal/jobs"
)

// This file holds the model-facing tools over the unified background-job
// registry (internal/jobs): job_list, job_output, and job_kill work for every
// producer kind — background shell commands (run_shell background=true) and
// background sub-agents (spawn_subagent) share the one id space. shell_output
// (tools/shell_bg.go) predates them and stays as the thin shell-only wrapper
// the model is used to; it shares the helpers below.

// listJobsText renders the registry one job per line, optionally restricted to
// one producer kind (shell_output lists only its own section).
func listJobsText(only jobs.Kind) string {
	var list []*jobs.Job
	for _, j := range jobs.Default().List() {
		if only != "" && j.Kind() != only {
			continue
		}
		list = append(list, j)
	}
	if len(list) == 0 {
		return "no background jobs"
	}
	var b strings.Builder
	for _, j := range list {
		fmt.Fprintf(&b, "job %d [%s]: %s — %s\n", j.ID(), j.Kind(), j.StatusLine(), j.Label())
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderJobOutput renders one job's status line and accumulated output.
func renderJobOutput(j *jobs.Job) string {
	out := strings.TrimRight(j.Output(), "\n")
	if out == "" {
		return fmt.Sprintf("job %d: %s\n(no output yet)", j.ID(), j.StatusLine())
	}
	return fmt.Sprintf("job %d: %s\n%s", j.ID(), j.StatusLine(), out)
}

// jobArg parses the {"job": N} argument shared by job_output and job_kill.
func jobArg(args json.RawMessage) (int, error) {
	var a struct {
		Job int `json:"job"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return 0, fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Job == 0 {
		return 0, fmt.Errorf("job is required (use job_list to list background jobs)")
	}
	return a.Job, nil
}

// JobListTool lists every background job, both kinds, with status.
func JobListTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "job_list",
			Description: "List all background jobs — shell commands started with run_shell(background=true) and background sub-agents from spawn_subagent — with their ids, kinds, and statuses. Job ids share one space: any id works with job_output/job_kill.",
			Parameters: Schema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return listJobsText(""), nil
		},
	}
}

// JobOutputTool reads one background job of either kind.
func JobOutputTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "job_output",
			Description: "Read the accumulated output and status of any background job. For a shell job this is its combined stdout/stderr so far; for a sub-agent job it is the final report once finished, or a still-running note while it works.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"job": {Type: "number", Description: "Background job id (see job_list, or the id returned by run_shell/spawn_subagent)."},
				},
				Required: []string{"job"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			id, err := jobArg(args)
			if err != nil {
				return "", err
			}
			job, ok := jobs.Default().Get(id)
			if !ok {
				return "", fmt.Errorf("no background job %d (use job_list to list jobs)", id)
			}
			return renderJobOutput(job), nil
		},
	}
}

// JobKillTool requests cancellation of a running background job.
func JobKillTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "job_kill",
			Description: "Stop a running background job by id. A shell job's whole process group is killed; a sub-agent job is cancelled and reports whatever partial results it has. Returns immediately — the job settles as killed once its work actually stops, and its completion notification still arrives.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"job": {Type: "number", Description: "Background job id (see job_list)."},
				},
				Required: []string{"job"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			id, err := jobArg(args)
			if err != nil {
				return "", err
			}
			job, err := jobs.Default().Cancel(id)
			if err != nil {
				return "", err
			}
			if job.State() != jobs.StatusRunning {
				return fmt.Sprintf("job %d had already finished (%s)", id, job.StatusLine()), nil
			}
			return fmt.Sprintf("requested cancellation of job %d [%s] (%s) — it settles as killed once stopped", id, job.Kind(), job.Label()), nil
		},
	}
}
