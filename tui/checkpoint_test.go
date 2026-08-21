package tui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func toolCall(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

// ckptWorkspace points the workspace root and the ocode state dir at throwaway
// temp dirs, so the shadow repo a test creates lands beside the test and never
// in the user's real config dir. Returns the workspace root.
func ckptWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// TestCheckpointBeforeCall locks in the hook direct tool calls and sub-agent
// runs share: a mutating call snapshots the workspace into the current turn, so
// after the mutations land /undo restores what the turn changed, removes what
// it created, and brings back what it deleted. Read-only calls snapshot
// nothing, so a turn that only reads never pays for a snapshot.
func TestCheckpointBeforeCall(t *testing.T) {
	dir := ckptWorkspace(t)
	edited := filepath.Join(dir, "a.txt")
	doomed := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(edited, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doomed, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Model{}
	before := m.checkpointBeforeCall()

	before(toolCall("read_file", `{"path":"`+edited+`"}`))
	m.ckpt.mu.Lock()
	pending := m.ckpt.pending
	m.ckpt.mu.Unlock()
	if pending != "" {
		t.Fatal("read_file should not be snapshotted")
	}

	// A delegated write, a delegated create, and a delegated delete.
	before(toolCall("write_file", `{"path":"`+edited+`","content":"changed"}`))
	if err := os.WriteFile(edited, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(dir, "sub", "new.txt")
	before(toolCall("write_file", `{"path":"`+created+`","content":"hello"}`))
	if err := os.MkdirAll(filepath.Dir(created), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	before(toolCall("delete_file", `{"path":"`+doomed+`"}`))
	if err := os.Remove(doomed); err != nil {
		t.Fatal(err)
	}

	m.finalizeCheckpoint("delegated turn")
	summary, touched := m.undoLast()
	t.Log(summary)
	if len(touched) != 3 {
		t.Fatalf("expected undo to touch 3 files, got %v", touched)
	}
	if got, _ := os.ReadFile(edited); string(got) != "original" {
		t.Fatalf("undo did not restore a.txt, got %q", got)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("undo did not remove created file sub/new.txt (err=%v)", err)
	}
	// Deleting a file used to be unrecoverable unless the tool announced the
	// path; the tree has it either way, mode bit included.
	info, err := os.Stat(doomed)
	if err != nil {
		t.Fatalf("undo did not bring back the deleted file: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("undo lost the exec bit: mode %v", info.Mode().Perm())
	}
}

// One snapshot per turn, taken before the first mutation: a second and third
// write in the same turn must not re-snapshot a mid-turn state, or /undo would
// rewind to v1 instead of the state the user last saw.
func TestCheckpointSnapshotsOncePerTurn(t *testing.T) {
	dir := ckptWorkspace(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{}
	before := m.checkpointBeforeCall()

	for _, v := range []string{"v1", "v2"} {
		before(toolCall("edit_file", `{"path":"`+f+`","old_string":"x","new_string":"`+v+`"}`))
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m.finalizeCheckpoint("two writers")
	m.undoLast()
	if got, _ := os.ReadFile(f); string(got) != "v0" {
		t.Fatalf("undo must restore the pre-turn v0, got %q", got)
	}
}

// The snapshot repo is ours, not theirs. Its git dir, index and refs live under
// the ocode state dir, so a turn plus an /undo must leave the user's own
// repository — staged work included — exactly as they left it.
func TestCheckpointLeavesTheUsersRepoAlone(t *testing.T) {
	dir := ckptWorkspace(t)
	userGit := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	userGit("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	userGit("add", "staged.txt")

	m := &Model{}
	before := m.checkpointBeforeCall()
	f := filepath.Join(dir, "a.txt")
	before(toolCall("write_file", `{"path":"`+f+`","content":"hi"}`))
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.finalizeCheckpoint("turn")
	m.undoLast()

	if got := userGit("diff", "--cached", "--name-only"); got != "staged.txt" {
		t.Fatalf("the user's index changed: staged = %q, want \"staged.txt\"", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index")); err != nil {
		t.Fatalf("the user's .git was disturbed: %v", err)
	}
}

// /undo reverts the files, but the model's own tool results for that turn stay
// in context describing the old state. Without a notice it keeps building on
// them, and edit_file's fuzzy tier matches the reverted file instead of failing
// clean. A no-op undo must stay silent — an advisory about nothing is noise.
func TestUndoTellsTheModelWhatWasReverted(t *testing.T) {
	dir := ckptWorkspace(t)
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
	dir := ckptWorkspace(t)
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("V1"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	m.snapshotBeforeMutate()
	if err := os.WriteFile(p, []byte("V2"), 0o644); err != nil {
		t.Fatal(err)
	}
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
