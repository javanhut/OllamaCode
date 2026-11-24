package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func verifyWorkspace(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	tools.SetWorkspaceRoot(dir)
	t.Cleanup(func() { tools.SetWorkspaceRoot("") })
}

// A run that changed files may not end on a failing check: the failure goes
// back to the model, and the run ends only once the check passes.
func TestRun_VerifyGateHandsFailureBack(t *testing.T) {
	verifyWorkspace(t)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("write_file", `{"path":"a.txt","content":"x"}`),
		textResp("done"), // check fails: ok.txt missing
		toolResp("write_file", `{"path":"ok.txt","content":"y"}`),
		textResp("done, verified"),
	}}
	res, err := Run(context.Background(), host, tools.DefaultRegistry(), "task",
		Options{Model: "m", MaxSteps: 8, Verify: true, VerifyCmd: "test -f ok.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "done, verified" || res.Verification != "passed" || res.VerifyFailures != 1 {
		t.Fatalf("output=%q verification=%q failures=%d", res.Output, res.Verification, res.VerifyFailures)
	}
	handedBack := false
	for _, m := range res.Messages {
		if m.Advisory && strings.Contains(m.Content, "[VERIFICATION FAILED]") {
			handedBack = true
		}
	}
	if !handedBack {
		t.Fatal("the failure was never shown to the model")
	}
}

func TestRun_VerifyGateGivesUpAfterBudget(t *testing.T) {
	verifyWorkspace(t)
	responses := []api.ChatResponse{toolResp("write_file", `{"path":"a.txt","content":"x"}`)}
	for range maxVerifyAttempts + 1 {
		responses = append(responses, textResp("done"))
	}
	host := &fakeChat{responses: responses}
	res, err := Run(context.Background(), host, tools.DefaultRegistry(), "task",
		Options{Model: "m", MaxSteps: 8, Verify: true, VerifyCmd: "false"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verification != "failed" || res.VerifyFailures != maxVerifyAttempts {
		t.Fatalf("verification=%q failures=%d", res.Verification, res.VerifyFailures)
	}
}

// No changes, nothing to verify: a read-only answer is never held up.
func TestRun_VerifyGateSkipsReadOnlyRuns(t *testing.T) {
	verifyWorkspace(t)
	host := &fakeChat{responses: []api.ChatResponse{textResp("42")}}
	res, err := Run(context.Background(), host, tools.DefaultRegistry(), "task",
		Options{Model: "m", Verify: true, VerifyCmd: "false"})
	if err != nil || res.Output != "42" || res.Verification != "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// When the step limit ends a run that changed files, the forced report is told
// the real verdict, so it cannot claim success on code that fails its check.
func TestRun_ForcedReportCarriesVerificationVerdict(t *testing.T) {
	verifyWorkspace(t)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("write_file", `{"path":"a.txt","content":"x"}`),
		textResp("All done, everything passes."), // forced report after MaxSteps=1
	}}
	res, err := Run(context.Background(), host, tools.DefaultRegistry(), "task",
		Options{Model: "m", MaxSteps: 1, Verify: true, VerifyCmd: "false"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HitLimit || res.Verification != "failed" {
		t.Fatalf("hitLimit=%v verification=%q", res.HitLimit, res.Verification)
	}
	prompt := res.Messages[len(res.Messages)-2]
	if !prompt.Advisory || !strings.Contains(prompt.Content, "NOT complete") {
		t.Fatalf("report prompt lacks the failing verdict: %q", prompt.Content)
	}
}
