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
