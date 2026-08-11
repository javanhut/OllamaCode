package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
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

func TestOpenFreshReplacesPriorDebugSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocode.log")
	first, err := OpenFresh(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Record(Event{Kind: "old_session"})
	_ = first.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := OpenFresh(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Record(Event{Kind: "new_session"})
	_ = second.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old_session") || !strings.Contains(string(data), "new_session") {
		t.Fatalf("fresh debug log was not replaced: %s", data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("debug log permissions = %v, %v; want 0600", info, err)
	}
}

func TestOpenFreshRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ocode.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if recorder, err := OpenFresh(link); err == nil {
		_ = recorder.Close()
		t.Fatal("OpenFresh followed a symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}

func TestRedactTextCoversLogShapedSecrets(t *testing.T) {
	in := "OPENAI_API_KEY=super-secret-value\npassword: hunter2\ntoken=abc123\ntoken := go-secret\nsk-1234567890abcdefghijklmnop\nghp_1234567890abcdefghijklmnop"
	got := RedactText(in)
	for _, secret := range []string{"super-secret-value", "hunter2", "abc123", "go-secret", "sk-1234567890abcdefghijklmnop", "ghp_1234567890abcdefghijklmnop"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q leaked in %q", secret, got)
		}
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

func TestRecordRequestLogsOnlyDeltas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defs := []tools.Tool{{Type: "function", Function: tools.Function{Name: "read_file", Description: "read a file"}}}
	format := map[string]any{"format": "{\"anyOf\":[]}"}
	msgs := []api.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}
	_ = r.RecordRequest(Event{Metadata: copyMeta(format)}, msgs, defs)
	msgs = append(msgs, api.Message{Role: "tool", Content: "envelope"})
	_ = r.RecordRequest(Event{Metadata: copyMeta(format)}, msgs, defs)
	// Compaction rewrites the history: everything from the first divergence on
	// has to be logged again.
	msgs = []api.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "summary"}}
	_ = r.RecordRequest(Event{Metadata: copyMeta(format)}, msgs, defs)
	_ = r.Close()

	var events []Event
	if err := Replay(path, func(ev Event) error { events = append(events, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	payloadLen := func(ev Event) int {
		var msgs []api.Message
		if err := json.Unmarshal(ev.Payload, &msgs); err != nil {
			t.Fatal(err)
		}
		return len(msgs)
	}
	if got := payloadLen(events[0]); got != 2 {
		t.Fatalf("first payload has %d messages, want 2", got)
	}
	if _, ok := events[0].Metadata["tool_definitions"]; !ok {
		t.Fatal("first request must carry the tool schemas")
	}
	if _, ok := events[0].Metadata["format"]; !ok {
		t.Fatal("first request must carry the constraint grammar")
	}
	if _, ok := events[0].Metadata["payload_from"]; ok {
		t.Fatal("a full payload must not claim an offset")
	}
	if got := payloadLen(events[1]); got != 1 {
		t.Fatalf("delta payload has %d messages, want 1", got)
	}
	if events[1].Metadata["payload_from"] != float64(2) {
		t.Fatalf("payload_from = %v, want 2", events[1].Metadata["payload_from"])
	}
	for _, key := range []string{"tool_definitions", "format"} {
		if _, ok := events[1].Metadata[key]; ok {
			t.Fatalf("unchanged %s must not be re-logged", key)
		}
	}
	// Only the system message survived the rewrite, so the payload restarts at 1.
	if got := payloadLen(events[2]); got != 1 {
		t.Fatalf("rewritten history logged %d messages, want 1", got)
	}
	if events[2].Metadata["payload_from"] != float64(1) {
		t.Fatalf("payload_from = %v, want 1", events[2].Metadata["payload_from"])
	}
	if events[2].Metadata["message_count"] != float64(2) {
		t.Fatalf("message_count = %v, want 2", events[2].Metadata["message_count"])
	}
}

func copyMeta(src map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Token counts matched the "token" secret pattern and were redacted, blinding
// metering while file contents stayed in cleartext. Numbers can't be secrets.
func TestRedactionKeepsNumericCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Record(Event{Kind: "model_response", Metadata: map[string]any{
		"prompt_tokens": 13599, "completion_tokens": 118, "api_key": "sk-live-abcdef", "auth_token": "hunter2",
	}})
	_ = r.Close()

	var got Event
	if err := Replay(path, func(ev Event) error { got = ev; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.Metadata["prompt_tokens"] != float64(13599) || got.Metadata["completion_tokens"] != float64(118) {
		t.Fatalf("token counts redacted: %#v", got.Metadata)
	}
	for _, key := range []string{"api_key", "auth_token"} {
		if got.Metadata[key] != "[REDACTED]" {
			t.Fatalf("%s leaked: %v", key, got.Metadata[key])
		}
	}
}
