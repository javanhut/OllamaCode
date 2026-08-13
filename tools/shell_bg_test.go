package tools

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

func TestBackgroundJobCountTracksLiveJobs(t *testing.T) {
	before := BackgroundJobCount()
	job, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := BackgroundJobCount(); got != before+1 {
		t.Fatalf("running job not counted: before=%d got=%d", before, got)
	}
	killShellCommand(job.cmd)
	if !waitJob(job, 3*time.Second) {
		t.Fatal("kill did not stop the job")
	}
	// The watcher marks the job done asynchronously; poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && BackgroundJobCount() != before {
		time.Sleep(20 * time.Millisecond)
	}
	if got := BackgroundJobCount(); got != before {
		t.Fatalf("exited job still counted: before=%d got=%d", before, got)
	}
}

func TestListBgJobsAndKillBgJob(t *testing.T) {
	job, err := startBackgroundShell("printf listed-output; sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}

	infos := ListBgJobs()
	var info *BgJobInfo
	for i := range infos {
		if infos[i].ID == job.id {
			info = &infos[i]
		}
	}
	if info == nil {
		t.Fatalf("job %d missing from ListBgJobs", job.id)
	}
	if info.Done {
		t.Error("running job reported done")
	}
	if info.PID <= 0 || info.Command == "" || info.Started.IsZero() {
		t.Errorf("snapshot fields not populated: %+v", info)
	}
	// Sorted by ID.
	for i := 1; i < len(infos); i++ {
		if infos[i-1].ID >= infos[i].ID {
			t.Fatalf("ListBgJobs not sorted by ID: %v", infos)
		}
	}

	// Wait for the reader goroutine to drain the pipe before killing, so the
	// output assertion below doesn't race the buffer fill.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(job.out.String(), "listed-output") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if err := KillBgJob(job.id); err != nil {
		t.Fatal(err)
	}
	if !waitJob(job, 3*time.Second) {
		t.Fatal("KillBgJob did not stop the job")
	}

	// Finished jobs stay listed, with their output tail.
	deadline = time.Now().Add(3 * time.Second)
	for {
		infos = ListBgJobs()
		info = nil
		for i := range infos {
			if infos[i].ID == job.id {
				info = &infos[i]
			}
		}
		if info != nil && info.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d never showed done: %+v", job.id, info)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(info.Output, "listed-output") {
		t.Errorf("snapshot missing buffered output tail: %q", info.Output)
	}

	// Killing a finished job is a documented no-op; an unknown ID is an error.
	if err := KillBgJob(job.id); err != nil {
		t.Errorf("killing a done job must be a no-op, got %v", err)
	}
	if err := KillBgJob(1 << 30); err == nil {
		t.Error("unknown job ID must be an error")
	}
}

func TestKillAllBackgroundJobs(t *testing.T) {
	j1, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	j2, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}

	KillAllBackgroundJobs()

	// KillAll marks jobs done synchronously and reaps every live job —
	// including any stragglers from earlier tests — so the live count is 0.
	if got := BackgroundJobCount(); got != 0 {
		t.Fatalf("live jobs survived KillAllBackgroundJobs: got=%d", got)
	}
	for _, j := range []*bgJob{j1, j2} {
		j.mu.Lock()
		done := j.done
		j.mu.Unlock()
		if !done {
			t.Errorf("job %d not marked done", j.id)
		}
	}

	// Idempotent: a second call must not panic or revive anything.
	KillAllBackgroundJobs()
	if got := BackgroundJobCount(); got != 0 {
		t.Fatalf("second KillAllBackgroundJobs changed the count: got=%d", got)
	}
}
