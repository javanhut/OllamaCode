package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// TestRun_BeforeHookFiresBeforeDispatch locks in the ordering the TUI's /undo
// checkpoint relies on: Options.Before is forwarded to the executor and runs
// synchronously BEFORE the tool handler mutates anything (snapshot pre-mutate).
func TestRun_BeforeHookFiresBeforeDispatch(t *testing.T) {
	var order []string
	reg := tools.NewRegistry()
	reg.Register(tools.Tool{
		Function: tools.Function{
			Name: "write_file",
			Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{
				"path": {Type: "string"}, "content": {Type: "string"},
			}},
		},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			order = append(order, "handler")
			return "ok", nil
		},
	})
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("write_file", `{"path":"/tmp/x","content":"y"}`),
		textResp("done"),
	}}

	var seen tools.ToolCall
	res, err := Run(context.Background(), host, reg, "write the file", Options{
		Model: "m",
		Before: func(call tools.ToolCall) {
			order = append(order, "before")
			seen = call
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "done" {
		t.Fatalf("output=%q", res.Output)
	}
	if len(order) != 2 || order[0] != "before" || order[1] != "handler" {
		t.Fatalf("Before must run exactly once and before the handler, got %v", order)
	}
	if seen.Function.Name != "write_file" {
		t.Fatalf("Before saw call %q, want write_file", seen.Function.Name)
	}
}
