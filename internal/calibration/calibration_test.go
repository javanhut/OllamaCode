package calibration

import (
	"context"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

type fakeClient struct {
	responses []api.ChatResponse
	index     int
}

func (f *fakeClient) ChatOnce(context.Context, api.ChatRequest) (api.ChatResponse, error) {
	r := f.responses[f.index]
	f.index++
	return r, nil
}

func TestRunRecommendsStrongForExactBehavior(t *testing.T) {
	call := func(name, args string) api.ChatResponse {
		return api.ChatResponse{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: name, Arguments: []byte(args)}}}}}
	}
	client := &fakeClient{responses: []api.ChatResponse{call("inspect_file", `{"path":"main.go"}`), call("web_lookup", `{"query":"official documentation"}`), {Message: api.Message{Content: "4"}}}}
	result, err := Run(context.Background(), client, "model", "provider", "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if result.Recommended != "strong" || result.Correct != 3 || result.ValidArgs != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCacheKeyChangesWithRuntime(t *testing.T) {
	if CacheKey("m", "p", "one", "d") == CacheKey("m", "p", "two", "d") {
		t.Fatal("runtime was not included")
	}
	if CacheKey("m", "p", "one", "d1") == CacheKey("m", "p", "one", "d2") {
		t.Fatal("digest was not included")
	}
}
