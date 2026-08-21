package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

// The session's stale-edit guard now lives in the tools layer (the TUI keeps
// one ledger and hands it to every dispatch), but the wiring guarantee is the
// TUI's: read, drift, edit through invokeTool must be refused.
func TestInvokeToolRefusesStaleEdit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Model{tools: tools.DefaultRegistry(), cfg: config{Host: DefaultHost}}
	call := func(name string, args map[string]any) string {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return m.invokeTool(context.Background(), tools.ToolCall{
			Function: tools.ToolCallFunction{Name: name, Arguments: raw},
		}).Content
	}

	call("read_file", map[string]any{"path": p})
	if err := os.WriteFile(p, []byte("externally rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := call("edit_file", map[string]any{"path": p, "old_string": "one", "new_string": "two"})
	if !strings.Contains(result, "changed on disk") {
		t.Fatalf("stale edit was not refused through invokeTool: %q", result)
	}

	// Re-reading is what clears the refusal: afterwards the same edit goes through.
	call("read_file", map[string]any{"path": p})
	result = call("edit_file", map[string]any{"path": p, "old_string": "externally rewritten", "new_string": "two"})
	if strings.Contains(result, "changed on disk") {
		t.Fatalf("edit after a fresh read was refused: %q", result)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "two\n" {
		t.Fatalf("edit after re-read did not land: %q", data)
	}
}
