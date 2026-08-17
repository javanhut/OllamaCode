package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

func readCall(path string) tools.ToolCall {
	args, _ := json.Marshal(map[string]string{"path": path})
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: "read_file", Arguments: args}}
}

func editCall(path string) tools.ToolCall {
	args, _ := json.Marshal(map[string]string{"path": path, "old_string": "a", "new_string": "b"})
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: "edit_file", Arguments: args}}
}

// The case this guard exists for: the model reads a file, the user edits it in
// their editor, and the model then edits from its stale copy.
func TestStaleEditIsRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("package main\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})

	// Nothing changed yet: the edit must go through untouched.
	if reason := m.requireFreshRead("edit_file", []string{p}); reason != "" {
		t.Fatalf("unchanged file was refused: %s", reason)
	}

	os.WriteFile(p, []byte("package main\n\nfunc userAdded() {}\n"), 0o644)
	reason := m.requireFreshRead("edit_file", []string{p})
	if reason == "" {
		t.Fatal("stale edit was allowed")
	}
	if !strings.Contains(reason, "changed on disk") || !strings.Contains(reason, "edit_file") {
		t.Fatalf("unhelpful refusal: %s", reason)
	}
}

// A model that ignores the refusal must not be refused forever with the same
// text; re-reading is what clears it, and the guard says so once.
func TestStaleRefusalIsNotSticky(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})
	os.WriteFile(p, []byte("two\n"), 0o644)

	if m.requireFreshRead("edit_file", []string{p}) == "" {
		t.Fatal("expected the first call to be refused")
	}
	if reason := m.requireFreshRead("edit_file", []string{p}); reason != "" {
		t.Fatalf("refused twice for the same drift: %s", reason)
	}
}

// The model's own successful write is not third-party drift.
func TestOwnWriteIsNotStale(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})
	os.WriteFile(p, []byte("two\n"), 0o644) // the model's edit landed
	m.rememberMutatedHashes([]string{p})

	if reason := m.requireFreshRead("edit_file", []string{p}); reason != "" {
		t.Fatalf("the model's own edit tripped the guard: %s", reason)
	}
}

// A file the model never read is not this guard's business —
// requireReadBeforeEdit owns that question, and gating here would block every
// first write in a session.
func TestUnreadFilePasses(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.go")
	os.WriteFile(p, []byte("x\n"), 0o644)

	m := &Model{}
	if reason := m.requireFreshRead("write_file", []string{p}); reason != "" {
		t.Fatalf("unread file was refused: %s", reason)
	}
}

// Path spelling must not defeat the ledger: the read and the edit can name the
// same file differently, exactly as they do for checkpoints.
func TestStaleGuardNormalizesPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})
	os.WriteFile(p, []byte("two\n"), 0o644)

	spelled := filepath.Join(dir, ".", "a.go")
	if m.requireFreshRead("edit_file", []string{spelled}) == "" {
		t.Fatal("a differently spelled path evaded the guard")
	}
}

// The ledger deliberately outlives a turn, so an edit based on a read from an
// earlier turn is still checked.
func TestStaleGuardSurvivesTurnReset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{todos: &todoList{}, failedCalls: map[string]int{}, turnReads: map[string]int{}}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})
	m.resetTurnGuards()
	os.WriteFile(p, []byte("two\n"), 0o644)

	if m.requireFreshRead("edit_file", []string{p}) == "" {
		t.Fatal("the ledger was cleared by the turn reset")
	}
}

// A deleted file drops its baseline rather than comparing a recreated file
// against a hash for bytes that are gone.
func TestDeletedFileDropsBaseline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{readCall(p)})
	os.Remove(p)
	m.rememberMutatedHashes([]string{p})

	if _, ok := m.readHashes[filepath.Clean(p)]; ok {
		t.Fatal("baseline survived deletion")
	}
	os.WriteFile(p, []byte("recreated\n"), 0o644)
	if reason := m.requireFreshRead("write_file", []string{p}); reason != "" {
		t.Fatalf("recreated file was refused: %s", reason)
	}
}

// Only the read tools seed the ledger; an edit call is not a read.
func TestOnlyReadsSeedTheLedger(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("one\n"), 0o644)

	m := &Model{}
	m.recordReadHashes([]tools.ToolCall{editCall(p)})
	if len(m.readHashes) != 0 {
		t.Fatalf("edit_file seeded the read ledger: %v", m.readHashes)
	}
}
