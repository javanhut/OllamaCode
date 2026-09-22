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

// The background path spawns its own child, so the scrub has to hold there too.
func TestBackgroundShellDropsSecretsFromChildEnvironment(t *testing.T) {
	t.Setenv("OCODE_FAKE_API_KEY", "planted-secret-value")
	job, err := startBackgroundShell("env", "", "")
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
	if strings.Contains(out, "planted-secret-value") {
		t.Error("background run_shell leaked a credential into job output")
	}
	if !strings.Contains(out, "PATH=") {
		t.Errorf("PATH must survive the scrub:\n%s", out)
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

type bgNotification struct {
	id   int
	code int
	err  error
	tail string
}

// captureNotifier installs a completion hook that records every invocation.
// Other tests leave background jobs running (e.g. the sleep above), so
// helpers filter by job id to ignore their stray notifications.
func captureNotifier(t *testing.T) chan bgNotification {
	t.Helper()
	notes := make(chan bgNotification, 8)
	SetBackgroundShellNotifier(func(jobID, exitCode int, err error, tail string) {
		notes <- bgNotification{jobID, exitCode, err, tail}
	})
	t.Cleanup(func() { SetBackgroundShellNotifier(nil) })
	return notes
}

func awaitNotification(t *testing.T, notes chan bgNotification, id int) bgNotification {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-notes:
			if n.id == id {
				return n
			}
		case <-deadline:
			t.Fatalf("notifier did not fire for job %d", id)
			return bgNotification{}
		}
	}
}

func assertNoSecondNotification(t *testing.T, notes chan bgNotification, id int) {
	t.Helper()
	select {
	case n := <-notes:
		if n.id == id {
			t.Fatalf("notifier fired twice for job %d: %+v", id, n)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// The completion hook fires exactly once on a normal exit, with the exit code
// and a tail of the output.
func TestBackgroundShellNotifierFiresOnceOnExit(t *testing.T) {
	notes := captureNotifier(t)
	job, err := startBackgroundShell("printf 'done-line\n'", "", "")
	if err != nil {
		t.Fatal(err)
	}
	n := awaitNotification(t, notes, job.id)
	if n.code != 0 || n.err != nil {
		t.Fatalf("expected clean exit, got code=%d err=%v", n.code, n.err)
	}
	if !strings.Contains(n.tail, "done-line") {
		t.Fatalf("tail missing output: %q", n.tail)
	}
	assertNoSecondNotification(t, notes, job.id)
}

// A kill funnels through the same cmd.Wait as a natural exit, so the hook
// still fires exactly once — never zero (lost) or twice (kill + exit).
func TestBackgroundShellNotifierFiresOnceOnKill(t *testing.T) {
	notes := captureNotifier(t)
	job, err := startBackgroundShell("sleep 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"job": job.id, "kill": true})
	if _, err := ShellOutputTool().Handler(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	n := awaitNotification(t, notes, job.id)
	if n.code == 0 && n.err == nil {
		t.Fatal("killed job must not report a clean exit")
	}
	assertNoSecondNotification(t, notes, job.id)
}

// The notification tail is bounded to the last bgNotifyTailLines lines.
func TestBackgroundShellNotifierTailIsBounded(t *testing.T) {
	notes := captureNotifier(t)
	job, err := startBackgroundShell("seq 1 30", "", "")
	if err != nil {
		t.Fatal(err)
	}
	n := awaitNotification(t, notes, job.id)
	lines := strings.Split(n.tail, "\n")
	if len(lines) != bgNotifyTailLines || lines[0] != "11" || lines[len(lines)-1] != "30" {
		t.Fatalf("expected the last %d lines (11..30), got %q", bgNotifyTailLines, n.tail)
	}
}
