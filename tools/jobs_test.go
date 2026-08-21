package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/javanhut/ollama_code/internal/jobs"
)

// registerFakeSubagent adds a subagent-kind job to the shared registry the way
// tui/subagent_bg.go does, without standing up a Model: output is a
// still-running note until Finish is called with the final report.
func registerFakeSubagent(label string) (*jobs.Job, *string, *bool) {
	report := ""
	cancelled := false
	var rec *jobs.Job
	rec = jobs.Default().Register(jobs.KindSubagent, label, jobs.Hooks{
		Status: func() string {
			if rec.State() == jobs.StatusRunning {
				return "running (1s elapsed)"
			}
			return "done (1s)"
		},
		Output: func() string {
			if rec.State() == jobs.StatusRunning {
				return "(still running, 1s elapsed)"
			}
			return report
		},
		Cancel: func() { cancelled = true },
	})
	return rec, &report, &cancelled
}

func callTool(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	return tool.Handler(context.Background(), json.RawMessage(args))
}

// Job ids come from one counter shared by both kinds: a shell job and a
// sub-agent job never collide, and job_list shows both sections.
func TestJobListCoversBothKinds(t *testing.T) {
	shell, err := startBackgroundShell("sleep 5", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = jobs.Default().Cancel(shell.id) })
	sub, _, _ := registerFakeSubagent("investigate foo")

	if shell.id == sub.ID() {
		t.Fatalf("shell and sub-agent jobs collided on id %d", shell.id)
	}
	out, err := callTool(t, JobListTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	shellLine := fmt.Sprintf("job %d [shell]:", shell.id)
	subLine := fmt.Sprintf("job %d [subagent]:", sub.ID())
	if !strings.Contains(out, shellLine) || !strings.Contains(out, subLine) {
		t.Fatalf("job_list missing a kind:\n%s", out)
	}
	if !strings.Contains(out, "investigate foo") {
		t.Fatalf("job_list missing the sub-agent label:\n%s", out)
	}
}

// job_output works for a sub-agent job: a still-running note while live, the
// final report once done.
func TestJobOutputSubagentKind(t *testing.T) {
	sub, report, _ := registerFakeSubagent("summarize bar")

	out, err := callTool(t, JobOutputTool(), fmt.Sprintf(`{"job":%d}`, sub.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "still running") {
		t.Fatalf("expected a still-running note, got %q", out)
	}

	*report = "found the answer in bar.go"
	sub.Finish("")
	out, err = callTool(t, JobOutputTool(), fmt.Sprintf(`{"job":%d}`, sub.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "found the answer in bar.go") {
		t.Fatalf("expected the final report, got %q", out)
	}
}

// job_kill routes a sub-agent cancel through the registry: the hook fires and
// the producer's settle is rewritten to killed.
func TestJobKillSubagentKind(t *testing.T) {
	sub, report, cancelled := registerFakeSubagent("long task")

	out, err := callTool(t, JobKillTool(), fmt.Sprintf(`{"job":%d}`, sub.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "requested cancellation") {
		t.Fatalf("unexpected kill result %q", out)
	}
	if !*cancelled {
		t.Fatal("the producer cancel hook was not invoked")
	}
	*report = "(cancelled)\n\npartial"
	sub.Finish("cancelled")
	if sub.State() != jobs.StatusKilled {
		t.Fatalf("expected killed after cancel+settle, got %s", sub.State())
	}

	// A second kill reports the terminal state instead of re-cancelling.
	out, err = callTool(t, JobKillTool(), fmt.Sprintf(`{"job":%d}`, sub.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "already finished") {
		t.Fatalf("expected already-finished, got %q", out)
	}
}

func TestJobKillShellKind(t *testing.T) {
	job, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := callTool(t, JobKillTool(), fmt.Sprintf(`{"job":%d}`, job.id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "requested cancellation") {
		t.Fatalf("unexpected kill result %q", out)
	}
	if !waitJob(job, 3*time.Second) {
		t.Fatal("kill did not stop the job")
	}
	if job.rec.State() != jobs.StatusKilled {
		t.Fatalf("expected the registry record to settle as killed, got %s", job.rec.State())
	}
}

func TestJobToolsRejectUnknownIDs(t *testing.T) {
	if _, err := callTool(t, JobOutputTool(), `{"job":424242}`); err == nil {
		t.Fatal("job_output accepted an unknown id")
	}
	if _, err := callTool(t, JobKillTool(), `{"job":424242}`); err == nil {
		t.Fatal("job_kill accepted an unknown id")
	}
	if _, err := callTool(t, JobOutputTool(), `{}`); err == nil {
		t.Fatal("job_output without a job id should error")
	}
}

// shell_output stays the model's habit: it reads the shell section of the same
// registry, with the same ids the model already sees.
func TestShellOutputBackwardCompatibility(t *testing.T) {
	job, err := startBackgroundShell("printf compat-ok; sleep 0.2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sub, _, _ := registerFakeSubagent("other kind")
	if job.id == sub.ID() {
		t.Fatal("id collision between shell and sub-agent jobs")
	}

	// Listing shows shell jobs but not sub-agent jobs.
	out, err := callTool(t, ShellOutputTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, fmt.Sprintf("job %d [shell]:", job.id)) {
		t.Fatalf("shell job missing from shell_output list:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("job %d ", sub.ID())) {
		t.Fatalf("sub-agent job leaked into the shell_output list:\n%s", out)
	}

	// Reading by id works exactly as before.
	if !waitJob(job, 3*time.Second) {
		t.Fatal("job did not finish")
	}
	out, err = callTool(t, ShellOutputTool(), fmt.Sprintf(`{"job":%d}`, job.id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "compat-ok") || !strings.Contains(out, "exited 0") {
		t.Fatalf("unexpected shell_output result: %q", out)
	}

	// A sub-agent id is rejected with a pointer at job_output.
	if _, err := callTool(t, ShellOutputTool(), fmt.Sprintf(`{"job":%d}`, sub.ID())); err == nil ||
		!strings.Contains(err.Error(), "job_output") {
		t.Fatalf("expected a kind-mismatch error pointing at job_output, got %v", err)
	}
}

// The job tools ship in the default registry, so both the TUI and headless
// runs expose them.
func TestJobToolsInDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()
	for _, name := range []string{"job_list", "job_output", "job_kill", "shell_output"} {
		if _, err := r.Invoke(context.Background(), ToolCall{
			Function: ToolCallFunction{Name: name, Arguments: json.RawMessage(`{"job":1}`)},
		}); err != nil && strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s not registered in DefaultRegistry", name)
		}
	}
}
