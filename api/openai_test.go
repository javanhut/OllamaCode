package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

func call(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestToOpenAIMessagesCorrelatesToolIDs(t *testing.T) {
	got := toOpenAIMessages([]Message{
		{Role: "user", Content: "read both"},
		{Role: "assistant", ToolCalls: []tools.ToolCall{call("read_file", `{"path":"a"}`), call("read_file", `{"path":"b"}`)}},
		{Role: "tool", ToolName: "read_file", Content: "A"},
		{Role: "tool", ToolName: "read_file", Content: "B"},
	})

	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	calls := got[1].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}
	if calls[0].ID == "" || calls[0].ID == calls[1].ID {
		t.Fatalf("tool call ids not distinct: %q, %q", calls[0].ID, calls[1].ID)
	}
	// Arguments must be a JSON *string* on the wire, not an object.
	if calls[0].Function.Arguments != `{"path":"a"}` {
		t.Errorf("arguments = %q, want the raw JSON string", calls[0].Function.Arguments)
	}
	// The Nth result answers the Nth call — swapping these silently misattributes
	// every tool result in the conversation.
	if got[2].ToolCallID != calls[0].ID || got[2].Content != "A" {
		t.Errorf("result A -> %q, want %q", got[2].ToolCallID, calls[0].ID)
	}
	if got[3].ToolCallID != calls[1].ID || got[3].Content != "B" {
		t.Errorf("result B -> %q, want %q", got[3].ToolCallID, calls[1].ID)
	}
}

// The loop guards splice system advisories between an assistant's tool calls and
// their results; the correlation has to see through them.
func TestToOpenAIMessagesSkipsInterleavedSystem(t *testing.T) {
	got := toOpenAIMessages([]Message{
		{Role: "assistant", ToolCalls: []tools.ToolCall{call("grep", `{}`)}},
		{Role: "system", Content: "[REPEATING ACTION] ..."},
		{Role: "tool", ToolName: "grep", Content: "hit"},
	})
	if len(got[0].ToolCalls) != 1 {
		t.Fatal("tool call was dropped despite having a result")
	}
	if got[2].ToolCallID != got[0].ToolCalls[0].ID {
		t.Errorf("result not correlated across the system message")
	}
}

// Both halves of an unbalanced pair are rejected by real providers with a 400,
// and a truncated history window produces either half.
func TestToOpenAIMessagesDropsUnbalancedPairs(t *testing.T) {
	t.Run("orphaned result", func(t *testing.T) {
		got := toOpenAIMessages([]Message{
			{Role: "tool", ToolName: "read_file", Content: "stranded"},
			{Role: "user", Content: "hi"},
		})
		for _, m := range got {
			if m.Role == "tool" {
				t.Fatal("orphaned tool result was sent")
			}
		}
	})
	t.Run("call with no result yet", func(t *testing.T) {
		got := toOpenAIMessages([]Message{
			{Role: "assistant", Content: "let me look", ToolCalls: []tools.ToolCall{call("grep", `{}`)}},
		})
		if len(got[0].ToolCalls) != 0 {
			t.Fatal("unanswered tool call was sent")
		}
		if got[0].Content != "let me look" {
			t.Error("dropping the call must keep the assistant text")
		}
	})
}

// sse builds a response body in the wire format providers actually send.
func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func drain(t *testing.T, body string) ([]ChatResponse, error) {
	t.Helper()
	out := make(chan ChatResponse, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		streamOpenAI(context.Background(), strings.NewReader(body), out, errs)
	}()
	var got []ChatResponse
	for c := range out {
		got = append(got, c)
	}
	return got, <-errs
}

// The failure this test exists for: tool-call arguments arrive as fragments that
// must be concatenated per index. Parsing any single fragment yields invalid JSON.
func TestStreamOpenAIAccumulatesFragmentedToolCalls(t *testing.T) {
	got, err := drain(t, sse(
		`{"choices":[{"delta":{"content":"looking"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"grep","arguments":"{\"q\":\"x\"}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
	))
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want a content chunk and a terminal chunk", len(got))
	}
	if got[0].Message.Content != "looking" {
		t.Errorf("first chunk content = %q, want %q", got[0].Message.Content, "looking")
	}

	final := got[len(got)-1]
	if !final.Done {
		t.Fatal("last chunk is not marked done")
	}
	if len(final.Message.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(final.Message.ToolCalls))
	}
	first := final.Message.ToolCalls[0]
	if first.Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", first.Function.Name)
	}
	if string(first.Function.Arguments) != `{"path":"a.go"}` {
		t.Errorf("arguments = %q, want the reassembled object", first.Function.Arguments)
	}
	var parsed map[string]string
	if err := json.Unmarshal(first.Function.Arguments, &parsed); err != nil {
		t.Errorf("reassembled arguments are not valid JSON: %v", err)
	}
	if final.Message.ToolCalls[1].Function.Name != "grep" {
		t.Errorf("second call = %q, want grep (index order)", final.Message.ToolCalls[1].Function.Name)
	}
	if final.PromptEval != 11 || final.EvalCount != 22 {
		t.Errorf("usage = %d/%d, want 11/22", final.PromptEval, final.EvalCount)
	}
}

func TestStreamOpenAISeparatesReasoning(t *testing.T) {
	got, err := drain(t, sse(
		`{"choices":[{"delta":{"reasoning":"hmm"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"still"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
	))
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	var thinking, content string
	for _, c := range got {
		thinking += c.Message.Thinking
		content += c.Message.Content
	}
	if thinking != "hmmstill" {
		t.Errorf("thinking = %q, want both provider keys captured", thinking)
	}
	if content != "answer" {
		t.Errorf("content = %q — reasoning must not leak into it", content)
	}
}

// Comments, blank lines and unparseable frames are routine on these streams and
// must not abort the turn.
func TestStreamOpenAIToleratesNoise(t *testing.T) {
	body := ": keep-alive\n\n" +
		"event: message\n" +
		`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
		"data: not json\n\n" +
		"data: [DONE]\n\n"
	got, err := drain(t, body)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(got) != 2 || got[0].Message.Content != "ok" || !got[1].Done {
		t.Errorf("got %+v, want one content chunk then done", got)
	}
}

// A stream cut off without [DONE] still has to terminate the turn rather than
// hang the caller waiting on a chunk that never comes.
func TestStreamOpenAITruncatedStreamStillFinishes(t *testing.T) {
	got, err := drain(t, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n")
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(got) == 0 || !got[len(got)-1].Done {
		t.Error("truncated stream did not produce a terminal chunk")
	}
}

func TestContinuousChatOpenAIEndToEnd(t *testing.T) {
	var body oaRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{"content":"hi"}}]}`)))
	}))
	defer server.Close()

	host := OllamaHost{}
	host.SetURI(server.URL) // no path: the /v1 suffix is supplied for us
	host.SetAPIKey("sk-test")
	host.SetProvider(ProviderOpenAI)

	temp := 0.2
	resp, errs := host.ContinuousChat(context.Background(), ChatRequest{
		Model:    "big",
		Messages: []Message{{Role: "user", Content: "hey"}},
		Options:  map[string]any{"num_ctx": 8192, "temperature": temp},
	})
	var content string
	for c := range resp {
		content += c.Message.Content
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if content != "hi" {
		t.Errorf("content = %q, want hi", content)
	}
	if !body.Stream || body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage must be set or token counts never arrive")
	}
	if body.Temperature == nil || *body.Temperature != temp {
		t.Error("temperature was not mapped from Options")
	}
	if body.Model != "big" {
		t.Errorf("model = %q, want big", body.Model)
	}
}

// A provider's error text is the only thing that explains a 400; swallowing it
// leaves schema and correlation mistakes undiagnosable.
func TestOpenAIErrorBodyIsSurfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"tool_call_id not found"}}`, http.StatusBadRequest)
	}))
	defer server.Close()

	host := OllamaHost{}
	host.SetURI(server.URL + "/v1")
	host.SetProvider(ProviderOpenAI)

	_, errs := host.ContinuousChat(context.Background(), ChatRequest{Model: "m"})
	err := <-errs
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "tool_call_id not found") {
		t.Errorf("error = %q, want the provider's message included", err)
	}
}
