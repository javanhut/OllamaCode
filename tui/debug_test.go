package tui

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

func TestEnableDebugRecordsSessionAndFullModelResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocode.log")
	m := &Model{modelName: "model-under-test"}
	if err := m.enableDebug(path, "test"); err != nil {
		t.Fatal(err)
	}
	m.streamThinking.WriteString("reasoning")
	m.recordModelResponse(4, "answer", []tools.ToolCall{{
		Function: tools.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"x.go"}`)},
	}}, 12, 3)
	_ = m.trace.Close()

	var kinds []string
	var response api.ChatResponse
	if err := tracepkg.Replay(path, func(event tracepkg.Event) error {
		kinds = append(kinds, event.Kind)
		if event.Kind == "model_response" {
			return json.Unmarshal(event.Payload, &response)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "session_start" || kinds[1] != "model_response" {
		t.Fatalf("unexpected debug events: %v", kinds)
	}
	if response.Message.Content != "answer" || response.Message.Thinking != "reasoning" ||
		len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("incomplete model response payload: %#v", response)
	}
}
