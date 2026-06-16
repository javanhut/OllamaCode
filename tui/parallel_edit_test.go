package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/javanhut/ollama_code/mcp"
)

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

// TestApplyStagedOp covers the safety-critical apply path: real edits land,
// a stale edit is rejected (conflict detection), new files are created, and
// every change is checkpointed so /undo can revert the whole batch.
func TestApplyStagedOp(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{tools: mcp.DefaultRegistry()}
	ctx := context.Background()

	if _, err := m.applyStagedOp(ctx, stagedOp{kind: "edit", path: f, oldString: "beta", newString: "BETA"}); err != nil {
		t.Fatalf("apply edit: %v", err)
	}
	if got, _ := os.ReadFile(f); string(got) != "alpha BETA gamma" {
		t.Fatalf("edit not applied, got %q", got)
	}

	// old_string is now gone — the real edit_file must reject it, which is how
	// the orchestrator detects a conflict between overlapping workers.
	if _, err := m.applyStagedOp(ctx, stagedOp{kind: "edit", path: f, oldString: "beta", newString: "x"}); err == nil {
		t.Fatal("expected conflict (stale old_string) to error, got nil")
	}

	g := filepath.Join(dir, "new.txt")
	if _, err := m.applyStagedOp(ctx, stagedOp{kind: "write", path: g, content: "hello"}); err != nil {
		t.Fatalf("apply write: %v", err)
	}
	if got, _ := os.ReadFile(g); string(got) != "hello" {
		t.Fatalf("write not applied, got %q", got)
	}

	// /undo restores the pre-batch state: a.txt reverts, new.txt is removed.
	m.finalizeCheckpoint("parallel_edit test")
	if _, touched := m.undoLast(); len(touched) == 0 {
		t.Fatal("expected undo to touch files")
	}
	if got, _ := os.ReadFile(f); string(got) != "alpha beta gamma" {
		t.Fatalf("undo did not restore a.txt, got %q", got)
	}
	if _, err := os.Stat(g); !os.IsNotExist(err) {
		t.Fatalf("undo did not remove created file new.txt (err=%v)", err)
	}
}

// TestStageEditValidation confirms a worker gets immediate feedback (and stages
// nothing) when its proposed edit can't be located unambiguously.
func TestStageEditValidation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("one two two"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &editStage{}
	tool := stageEditTool(st)
	ctx := context.Background()

	if _, err := tool.Handler(ctx, mustArgs(t, map[string]any{"path": f, "old_string": "zzz", "new_string": "x"})); err == nil {
		t.Fatal("expected error for old_string not found")
	}
	if _, err := tool.Handler(ctx, mustArgs(t, map[string]any{"path": f, "old_string": "two", "new_string": "x"})); err == nil {
		t.Fatal("expected ambiguity error (2 matches, replace_all unset)")
	}
	if len(st.list()) != 0 {
		t.Fatalf("nothing should be staged after validation failures, got %d", len(st.list()))
	}

	if _, err := tool.Handler(ctx, mustArgs(t, map[string]any{"path": f, "old_string": "one", "new_string": "1"})); err != nil {
		t.Fatalf("valid stage_edit errored: %v", err)
	}
	ops := st.list()
	if len(ops) != 1 || ops[0].kind != "edit" || ops[0].path != f || ops[0].newString != "1" {
		t.Fatalf("op not staged correctly: %+v", ops)
	}
}

// TestPlannerToolGate locks in that a planning worker can read and stage, but
// can never reach a real write/shell tool, recurse, or rebuild the index.
func TestPlannerToolGate(t *testing.T) {
	allow := []string{"read_file", "grep", "find_symbol", "semantic_search", "stage_edit", "stage_write", "stage_delete"}
	deny := []string{"write_file", "edit_file", "delete_file", "run_shell", "git_commit", "spawn_subagent", "parallel_edit", "code_index", "ask_user"}
	for _, n := range allow {
		if !plannerAllowed(n) {
			t.Errorf("planner should allow %q", n)
		}
	}
	for _, n := range deny {
		if plannerAllowed(n) {
			t.Errorf("planner must deny %q", n)
		}
	}

	m := &Model{tools: mcp.DefaultRegistry()}
	reg := m.plannerRegistry(&editStage{})
	for _, tdef := range reg.Definitions() {
		if !plannerAllowed(tdef.Function.Name) {
			t.Errorf("plannerRegistry exposed disallowed tool %q", tdef.Function.Name)
		}
	}
}
