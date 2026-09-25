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
	if f.index >= len(f.responses) {
		// Run appends ratio-measurement probes after the behavior probes; a
		// zero response reports no prompt_eval_count, so measurement is skipped.
		return api.ChatResponse{}, nil
	}
	r := f.responses[f.index]
	f.index++
	return r, nil
}

func TestRunRecommendsStrongForExactBehavior(t *testing.T) {
	call := func(name, args string) api.ChatResponse {
		return api.ChatResponse{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: name, Arguments: []byte(args)}}}}}
	}
	client := &fakeClient{responses: []api.ChatResponse{call("inspect_file", `{"path":"main.go"}`), call("web_lookup", `{"query":"official documentation"}`), {Message: api.Message{Content: "4"}}}}
	result, err := Run(context.Background(), client, "model", "provider", "runtime", 0)
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

type recordingClient struct{ numCtx []any }

func (r *recordingClient) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	r.numCtx = append(r.numCtx, req.Options["num_ctx"])
	return api.ChatResponse{PromptEval: 100}, nil
}

// Every probe, behavior and ratio alike, must carry the chat's num_ctx, or
// Ollama reloads the model for the probe and again for the next chat turn.
func TestRunPinsNumCtxOnEveryProbe(t *testing.T) {
	client := &recordingClient{}
	if _, err := Run(context.Background(), client, "model", "provider", "runtime", 65536); err != nil {
		t.Fatal(err)
	}
	if len(client.numCtx) < 4 {
		t.Fatalf("only %d probes seen", len(client.numCtx))
	}
	for i, v := range client.numCtx {
		if v != 65536 {
			t.Fatalf("probe %d num_ctx = %v, want 65536", i, v)
		}
	}
}
