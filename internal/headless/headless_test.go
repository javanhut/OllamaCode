package headless

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

// fakeChat returns scripted responses in order, recording the requests it saw
// (mirrors the fakeChat in internal/agent's tests).
type fakeChat struct {
	responses []api.ChatResponse
	requests  []api.ChatRequest
}

func (f *fakeChat) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	f.requests = append(f.requests, req)
	r := f.responses[len(f.requests)-1]
	return r, nil
}

func echoRegistry(seen *string) *tools.Registry {
	r := tools.NewRegistry()
	r.Register(tools.Tool{
		Function: tools.Function{
			Name:       "echo",
			Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{"text": {Type: "string"}}},
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

func TestRunDispatchesToolThenAnswers(t *testing.T) {
	var seen string
	host := &fakeChat{responses: []api.ChatResponse{
		{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}}}},
		{Message: api.Message{Content: "all done"}},
	}}
	res, err := Run(context.Background(), host, echoRegistry(&seen), "do it", Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "hi" {
		t.Fatalf("tool not dispatched, seen=%q", seen)
	}
	if res.Output != "all done" || res.Steps != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The default system prompt must be the headless one (non-interactive
	// posture), sent as the first message.
	first := host.requests[0].Messages[0]
	if first.Role != "system" || first.Content != DefaultSystem {
		t.Fatalf("system prompt = %q %q", first.Role, first.Content)
	}
}

func TestRunCustomSystem(t *testing.T) {
	host := &fakeChat{responses: []api.ChatResponse{{Message: api.Message{Content: "ok"}}}}
	_, err := Run(context.Background(), host, tools.NewRegistry(), "hi", Options{Model: "m", System: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if got := host.requests[0].Messages[0].Content; got != "custom" {
		t.Fatalf("system prompt = %q", got)
	}
}

func TestRunWritesDebugModelAndToolLifecycle(t *testing.T) {
	var seen string
	host := &fakeChat{responses: []api.ChatResponse{
		{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}}}},
		{Message: api.Message{Content: "all done"}},
	}}
	path := filepath.Join(t.TempDir(), "ocode.log")
	recorder, err := tracepkg.OpenFresh(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), host, echoRegistry(&seen), "do it", Options{Model: "m", Trace: recorder}); err != nil {
		t.Fatal(err)
	}
	_ = recorder.Close()

	kinds := map[string]int{}
	if err := tracepkg.Replay(path, func(event tracepkg.Event) error {
		kinds[event.Kind]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"turn_start", "model_request", "model_response", "tool", "turn_end"} {
		if kinds[kind] == 0 {
			t.Fatalf("debug log missing %q event: %#v", kind, kinds)
		}
	}
}

func TestRunPropagatesHostError(t *testing.T) {
	host := &errChat{}
	_, err := Run(context.Background(), host, tools.NewRegistry(), "hi", Options{Model: "m"})
	if err == nil {
		t.Fatal("expected the host error to propagate")
	}
}

type errChat struct{}

func (errChat) ChatOnce(context.Context, api.ChatRequest) (api.ChatResponse, error) {
	return api.ChatResponse{}, context.DeadlineExceeded
}

func TestReportJSONShape(t *testing.T) {
	res := agent.Result{
		Output: "done", Steps: 2, ToolCalls: 3, ToolErrors: 1,
		ToolsUsed: []string{"read_file"}, PromptTokens: 10, CompletionTokens: 20,
	}
	data, err := json.Marshal(NewReport(res, "qwen3:8b"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"output", "model", "steps", "tool_calls", "tool_errors", "tools_used", "prompt_tokens", "completion_tokens"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("report missing %q: %s", key, data)
		}
	}
	if got["output"] != "done" || got["model"] != "qwen3:8b" {
		t.Fatalf("unexpected report: %s", data)
	}
	// omitempty: no hit_limit key when the run finished naturally.
	if _, ok := got["hit_limit"]; ok {
		t.Fatalf("hit_limit should be omitted when false: %s", data)
	}
}

func TestReportHitLimitIncluded(t *testing.T) {
	data, _ := json.Marshal(NewReport(agent.Result{Output: "partial", HitLimit: true}, "m"))
	if !strings.Contains(string(data), `"hit_limit":true`) {
		t.Fatalf("hit_limit should appear when true: %s", data)
	}
}
