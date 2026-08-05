package trace

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

func TestRecorderRedactsAndReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Record(Event{Kind: "tool", Arguments: json.RawMessage(`{"api_key":"secret","nested":{"token":"hidden"},"path":"ok"}`), Result: "Authorization: Bearer abc.def"})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	var got Event
	if err := Replay(path, func(event Event) error { got = event; return nil }); err != nil {
		t.Fatal(err)
	}
	text := string(got.Arguments) + got.Result
	if strings.Contains(text, "secret") || strings.Contains(text, "hidden") || strings.Contains(text, "abc.def") {
		t.Fatalf("secret leaked: %s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("missing redaction: %s", text)
	}
}

func TestPromoteBuildsFixtureSkeleton(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.jsonl")
	r, _ := Open(path)
	_ = r.Record(Event{Kind: "turn_start", Metadata: map[string]any{"task": "fix it"}})
	_ = r.Record(Event{Kind: "tool", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x.go"}`)})
	_ = r.Record(Event{Kind: "tool", Tool: "read_file", Arguments: json.RawMessage(`{"path":"y.go"}`)})
	_ = r.Close()
	fixture, err := Promote(path)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Prompt != "fix it" || len(fixture.RequiredTools) != 1 || len(fixture.Calls) != 2 {
		t.Fatalf("unexpected fixture: %#v", fixture)
	}
}

func TestReplayClientReturnsCapturedResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.jsonl")
	r, _ := Open(path)
	payload, _ := json.Marshal(api.ChatResponse{Message: api.Message{Content: "captured"}})
	_ = r.Record(Event{Kind: "model_response", Payload: payload})
	_ = r.Close()
	client, err := NewReplayClient(path)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ChatOnce(context.Background(), api.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "captured" {
		t.Fatalf("unexpected response: %#v", response)
	}
}
