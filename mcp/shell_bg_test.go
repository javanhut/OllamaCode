package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func waitJob(job *bgJob, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		job.mu.Lock()
		done := job.done
		job.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestBackgroundShellRunsAndReports(t *testing.T) {
	job, err := startBackgroundShell("printf hello; sleep 0.2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !waitJob(job, 3*time.Second) {
		t.Fatal("job did not finish")
	}
	args, _ := json.Marshal(map[string]int{"job": job.id})
	out, err := ShellOutputTool().Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected output to contain hello, got %q", out)
	}
	if !strings.Contains(out, "exited 0") {
		t.Fatalf("expected exited 0 status, got %q", out)
	}
}

func TestBackgroundShellKill(t *testing.T) {
	job, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"job": job.id, "kill": true})
	if _, err := ShellOutputTool().Handler(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if !waitJob(job, 3*time.Second) {
		t.Fatal("kill did not stop the job")
	}
}

func TestRunShellBackgroundReturnsImmediately(t *testing.T) {
	start := time.Now()
	args, _ := json.Marshal(map[string]any{"command": "sleep 5", "background": true})
	res, err := RunShellTool().Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("background run_shell blocked for %s", time.Since(start))
	}
	if !strings.Contains(res, "started background job") {
		t.Fatalf("unexpected result: %q", res)
	}
}
