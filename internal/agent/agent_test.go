package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

// fakeChat returns scripted responses in order, recording the requests it saw.
type fakeChat struct {
	responses []api.ChatResponse
	calls     int
}

func (f *fakeChat) ChatOnce(_ context.Context, _ api.ChatRequest) (api.ChatResponse, error) {
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

func echoRegistry(seen *string) *mcp.Registry {
	r := mcp.NewRegistry()
	r.Register(mcp.Tool{
		Function: mcp.Function{
			Name:       "echo",
			Parameters: mcp.Schema{Type: "object", Properties: map[string]mcp.Property{"text": {Type: "string"}}},
		},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(args, &a)
			*seen = a.Text
			return "echoed: " + a.Text, nil
		},
	})
	return r
}

func toolResp(name, args string) api.ChatResponse {
	return api.ChatResponse{Message: api.Message{
		ToolCalls: []mcp.ToolCall{{Function: mcp.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}},
	}}
}

func textResp(s string) api.ChatResponse {
	return api.ChatResponse{Message: api.Message{Content: s}}
}

func TestRun_DispatchesToolThenAnswers(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"hi"}`),
		textResp("all done"),
	}}
	res, err := Run(context.Background(), host, reg, "do it", Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "hi" {
		t.Fatalf("tool not dispatched with args, seen=%q", seen)
	}
	if res.Output != "all done" {
		t.Fatalf("final output = %q", res.Output)
	}
	if res.Steps != 1 {
		t.Fatalf("expected 1 tool step, got %d", res.Steps)
	}
}

func TestRun_ToolFilterBlocks(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"hi"}`),
		textResp("done"),
	}}
	// Filter permits nothing -> the echo call must be refused, not executed.
	res, err := Run(context.Background(), host, reg, "do it", Options{
		Model:      "m",
		ToolFilter: func(string) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Fatalf("filtered tool should not have run, seen=%q", seen)
	}
	if res.Output != "done" {
		t.Fatalf("output=%q", res.Output)
	}
}

func TestRun_EscalatesBadArgsViaFormat(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Round 1: model emits invalid-JSON args -> Invoke fails validation ->
	// escalation asks for a fix (2nd response, schema-constrained) -> retry
	// succeeds. Round 2: model answers.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text": bad}`), // malformed
		textResp(`{"text":"fixed"}`),      // format-repair reply
		textResp("done"),                  // final answer
	}}
	res, err := Run(context.Background(), host, reg, "do it", Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "fixed" {
		t.Fatalf("escalation should have repaired args and dispatched; seen=%q", seen)
	}
	if res.Output != "done" {
		t.Fatalf("output=%q", res.Output)
	}
	if host.calls != 3 {
		t.Fatalf("expected 3 chat calls (call, repair, answer), got %d", host.calls)
	}
}

func TestRun_StuckGuardBreaksAndFinalizes(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// The model spams the identical call. Budget is 8, but the stuck-guard should
	// dispatch it only maxIdenticalCalls (2) times, refuse the 3rd, see no
	// progress, and break to a tool-less finalize — returning useful output well
	// before the step cap instead of the old "hit the limit" sentinel.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"same"}`), // dispatched (count 1)
		toolResp("echo", `{"text":"same"}`), // dispatched (count 2)
		toolResp("echo", `{"text":"same"}`), // refused -> no progress -> break
		textResp("final report"),            // finalize (no tools available)
	}}
	res, err := Run(context.Background(), host, reg, "task", Options{Model: "m", MaxSteps: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "final report" {
		t.Fatalf("expected finalized report, got %q", res.Output)
	}
	if !res.HitLimit {
		t.Fatal("expected HitLimit=true (broke via guard, didn't answer naturally)")
	}
	if res.Steps != 3 {
		t.Fatalf("guard should break at step 3, not run to the cap; got %d", res.Steps)
	}
	if host.calls != 4 {
		t.Fatalf("expected 4 chat calls (3 rounds + finalize), got %d", host.calls)
	}
}

func TestRun_StepLimit(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Always returns a tool call -> should hit the step cap.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"a"}`),
		toolResp("echo", `{"text":"b"}`),
		toolResp("echo", `{"text":"c"}`),
	}}
	res, err := Run(context.Background(), host, reg, "loop", Options{Model: "m", MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HitLimit {
		t.Fatal("expected HitLimit=true")
	}
	if res.Steps != 2 {
		t.Fatalf("expected 2 steps, got %d", res.Steps)
	}
}
