package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
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

// MutatedPaths returns the raw JSON argument, so one file can arrive under two
// spellings in a single turn ("./a.txt" and "a.txt"). Keyed raw that is two
// entries, the second holding the mid-turn state, and undoLast's map iteration
// picks a winner at random. One entry, holding the original, is the guarantee.
func TestCheckpointKeyIgnoresPathSpelling(t *testing.T) {
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
	// Same file, different spelling — must not mint a second snapshot of v1.
	before(toolCall("edit_file", `{"path":"`+dir+`/./a.txt","old_string":"v1","new_string":"v2"}`))
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.ckpt.mu.Lock()
	n := len(m.ckpt.pending)
	m.ckpt.mu.Unlock()
	if n != 1 {
		t.Fatalf("two spellings of one file produced %d snapshots, want 1", n)
	}
	m.finalizeCheckpoint("two spellings")
	m.undoLast()
	if got, _ := os.ReadFile(f); string(got) != "v0" {
		t.Fatalf("undo must restore the pre-turn v0, got %q", got)
	}
}

// /undo reverts the files, but the model's own tool results for that turn stay
// in context describing the old state. Without a notice it keeps building on
// them, and edit_file's fuzzy tier matches the reverted file instead of failing
// clean. A no-op undo must stay silent — an advisory about nothing is noise.
func TestUndoTellsTheModelWhatWasReverted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := statusTestModel()
	m.history = []api.Message{{Role: "user", Content: "edit a.txt"}}

	m.slashInput(t, "/undo") // empty stack
	if len(m.history) != 1 {
		t.Fatalf("undo with an empty stack appended %d message(s)", len(m.history)-1)
	}

	before := m.checkpointBeforeCall()
	before(toolCall("edit_file", `{"path":"`+f+`","old_string":"original","new_string":"changed"}`))
	if err := os.WriteFile(f, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.finalizeCheckpoint("edit a.txt")

	m.slashInput(t, "/undo")
	last := m.history[len(m.history)-1]
	if !last.Advisory || !strings.Contains(last.Content, f) {
		t.Fatalf("last message = %+v, want an advisory naming %s", last, f)
	}
}

// An /undo typed while a tool batch is in flight must not splice a user turn
// between the assistant's tool_calls message and the results that belong to it.
func TestUndoAdvisoryWaitsForBatch(t *testing.T) {
	sessionPersist.Store(false)
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("V1"), 0o644)

	call := tools.ToolCall{Function: tools.ToolCallFunction{Name: "read_file", Arguments: []byte(`{"path":"x"}`)}}
	m := &Model{
		history: []api.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []tools.ToolCall{call}},
		},
		pending: &pendingBatch{
			calls:   []tools.ToolCall{call},
			results: []api.Message{{Role: "tool", ToolName: "read_file", Content: "ok"}},
			started: []bool{true},
			done:    1,
		},
		todos: &todoList{},
	}
	m.snapshotBeforeMutate([]string{p})
	os.WriteFile(p, []byte("V2"), 0o644)
	m.finalizeCheckpoint("edit")
	_, touched := m.undoLast()
	m.noteUndoToModel(touched)

	if len(m.history) != 2 {
		t.Fatalf("advisory spliced into the live batch: %d messages", len(m.history))
	}
	if m.deferredAdvisory == "" {
		t.Fatal("advisory was dropped instead of deferred")
	}
	t.Log("deferred while the batch was open")

	m.history = append(m.history, m.pending.results...)
	m.flushDeferredAdvisory()

	roles := make([]string, len(m.history))
	for i, msg := range m.history {
		roles[i] = msg.Role
		if msg.Advisory {
			roles[i] += "/advisory"
		}
	}
	t.Logf("final order: %v", roles)
	if m.history[2].Role != "tool" {
		t.Fatal("tool result no longer follows its tool_calls message")
	}
	if !m.history[3].Advisory {
		t.Fatal("advisory did not land after the batch")
	}
	if m.deferredAdvisory != "" {
		t.Fatal("deferred advisory not cleared — it would repeat")
	}
}
