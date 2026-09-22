package tui

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

type parityClient struct {
	calls      int
	toolResult string
}

func (p *parityClient) ChatOnce(_ context.Context, request api.ChatRequest) (api.ChatResponse, error) {
	p.calls++
	if p.calls == 1 {
		return api.ChatResponse{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}}}}}, nil
	}
	for _, message := range request.Messages {
		if message.Role == "tool" {
			p.toolResult = message.Content
		}
	}
	return api.ChatResponse{Message: api.Message{Content: "done"}}, nil
}

func TestInteractiveAndHeadlessUseSameExecutionContract(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.Tool{Function: tools.Function{Name: "echo", Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{"text": {Type: "string"}}, Required: []string{"text"}}}, Handler: func(_ context.Context, args json.RawMessage) (string, error) {
		var value struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(args, &value)
		return "echo: " + value.Text, nil
	}})
	call := tools.ToolCall{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}}
	interactive := (&Model{tools: registry}).invokeTool(context.Background(), call).Content
	client := &parityClient{}
	if _, err := agent.Run(context.Background(), client, registry, "echo", agent.Options{Model: "fake", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if interactive != client.toolResult {
		t.Fatalf("execution contracts drifted:\ninteractive=%s\nheadless=%s", interactive, client.toolResult)
	}
	result, ok := tools.DecodeToolResult(interactive)
	if !ok || !result.OK {
		t.Fatalf("expected structured success: %s", interactive)
	}
}
