package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestShellCommandResultEmptyOutputIsExplicit(t *testing.T) {
	got, err := shellCommandResult("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[ok] (exit 0, no output)" {
		t.Fatalf("empty success must say so explicitly, got %q", got)
	}
	// A non-zero exit still surfaces as a CommandFailure.
	if _, err := shellCommandResult("", exitError(t, "exit 7")); err == nil {
		t.Fatal("expected a failure for a non-zero exit")
	}
}

// exitError runs command through the same shell resolution the tool uses and
// returns its (expected) error.
func exitError(t *testing.T, command string) error {
	t.Helper()
	_, err := runShellCommand(context.Background(), command, "", "", 5*time.Second)
	if err == nil {
		t.Fatalf("%q unexpectedly succeeded", command)
	}
	return err
}

// TestRunShellPipefailFailsPipeline is the regression test for the debug-log
// incident: `curl -s http://dead-server/ | head` reported ok:true because
// plain sh returns only the last pipeline stage's status. With pipefail a
// failing first stage must fail the command.
func TestRunShellPipefailFailsPipeline(t *testing.T) {
	if detectShell() != "bash" {
		t.Skip("bash not on PATH; pipefail unavailable")
	}
	_, err := runShellCommand(context.Background(), "exit 3 | head -1", "", "", 5*time.Second)
	var failed *CommandFailure
	if !errors.As(err, &failed) {
		t.Fatalf("pipeline with a failing first stage must fail, got err=%v", err)
	}
	if failed.ExitCode != 3 {
		t.Fatalf("expected exit 3 from the failing stage, got %d (%q)", failed.ExitCode, failed.Output)
	}

	// A healthy pipeline still succeeds and returns its output.
	out, err := runShellCommand(context.Background(), "echo hi | head -1", "", "", 5*time.Second)
	if err != nil || out != "hi" {
		t.Fatalf("healthy pipeline broke: out=%q err=%v", out, err)
	}
}

func TestStripBackgroundAmpersand(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		stripped bool
	}{
		{"npm start &", "npm start", true},
		{"npm start &  \n", "npm start", true},
		{"npm start&", "npm start", true},
		{"npm start", "npm start", false},
		{"echo a && echo b", "echo a && echo b", false},
		{"echo a &&", "echo a &&", false}, // broken AND-list, not backgrounding
		{"echo 'a & b'", "echo 'a & b'", false},
	}
	for _, c := range cases {
		got, stripped := stripBackgroundAmpersand(c.in)
		if got != c.want || stripped != c.stripped {
			t.Errorf("stripBackgroundAmpersand(%q) = (%q, %v), want (%q, %v)", c.in, got, stripped, c.want, c.stripped)
		}
	}
}

func TestRunShellBackgroundStripsTrailingAmpersand(t *testing.T) {
	bgMu.Lock()
	id := bgNext
	bgMu.Unlock()
	args, _ := json.Marshal(map[string]any{"command": "sleep 30 &", "background": true})
	res, err := RunShellTool().Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "stripped trailing '&'") {
		t.Fatalf("result must note the stripped '&', got %q", res)
	}
	job := lookupBgJob(id)
	if job == nil {
		t.Fatal("job was not registered")
	}
	defer func() {
		killShellCommand(job.cmd)
		waitJob(job, 3*time.Second)
	}()
	if strings.HasSuffix(strings.TrimSpace(job.command), "&") {
		t.Fatalf("command still ends with '&': %q", job.command)
	}

	// No '&' → no note.
	args, _ = json.Marshal(map[string]any{"command": "sleep 30", "background": true})
	res, err = RunShellTool().Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res, "stripped") {
		t.Fatalf("no '&' to strip, but result claims otherwise: %q", res)
	}
	if job2 := lookupBgJob(id + 1); job2 != nil {
		killShellCommand(job2.cmd)
	}
}
