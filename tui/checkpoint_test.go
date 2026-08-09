package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

func toolCall(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

// TestCheckpointBeforeCall locks in the hook direct tool calls and sub-agent
// runs share: a mutating call snapshots its target paths into the current
// turn, so after the mutation lands /undo restores the pre-turn content, and
// read-only calls snapshot nothing.
func TestCheckpointBeforeCall(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{}
	before := m.checkpointBeforeCall()

	// A read-only call must not bank anything.
	before(toolCall("read_file", `{"path":"`+f+`"}`))
	m.ckpt.mu.Lock()
	empty := len(m.ckpt.pending) == 0
	m.ckpt.mu.Unlock()
	if !empty {
		t.Fatal("read_file should not be snapshotted")
	}

	// Simulate a delegated write: snapshot via the hook, then mutate.
	before(toolCall("write_file", `{"path":"`+f+`","content":"changed"}`))
	if err := os.WriteFile(f, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A delegated create: snapshotted as not-existing, then created.
	g := filepath.Join(dir, "new.txt")
	before(toolCall("write_file", `{"path":"`+g+`","content":"hello"}`))
	if err := os.WriteFile(g, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.finalizeCheckpoint("delegated turn")
	if _, touched := m.undoLast(); len(touched) != 2 {
		t.Fatalf("expected undo to touch 2 files, got %v", touched)
	}
	if got, _ := os.ReadFile(f); string(got) != "original" {
		t.Fatalf("undo did not restore a.txt, got %q", got)
	}
	if _, err := os.Stat(g); !os.IsNotExist(err) {
		t.Fatalf("undo did not remove created file new.txt (err=%v)", err)
	}
}

// TestCheckpointFirstVersionWins covers parent+child (or two parallel children)
// touching the same file in one turn: the FIRST snapshot of a path wins, so
// /undo restores the true pre-turn content rather than a mid-turn state.
func TestCheckpointFirstVersionWins(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{}
	before := m.checkpointBeforeCall()

	before(toolCall("edit_file", `{"path":"`+f+`","old_string":"v0","new_string":"v1"}`))
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second mutating call on the same path must NOT re-snapshot v1.
	before(toolCall("edit_file", `{"path":"`+f+`","old_string":"v1","new_string":"v2"}`))
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.finalizeCheckpoint("two writers")
	m.undoLast()
	if got, _ := os.ReadFile(f); string(got) != "v0" {
		t.Fatalf("undo must restore the first snapshot v0, got %q", got)
	}
}
