package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
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
	// applyStagedOp goes through the real (workspace-jailed) fs tools, so the
	// temp dir must be the working root for its absolute paths to pass — and
	// the turn snapshot's shadow repo has to land in a temp state dir too.
	dir := ckptWorkspace(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{tools: tools.DefaultRegistry()}
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

// TestParallelEditRollbackOnConflict covers the atomic batch semantics: a
// mid-batch conflict aborts the apply, every file the batch already touched is
// restored to its pre-batch content (including removing files the batch
// created), the error names the failed op and the rolled-back changes, and the
// turn checkpoint is left intact so /undo still works afterwards.
func TestParallelEditRollbackOnConflict(t *testing.T) {
	// applyPlannedOps goes through the workspace-jailed fs tools, so the temp
	// dir must be the working root for its absolute paths to pass.
	dir := ckptWorkspace(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{tools: tools.DefaultRegistry()}
	ctx := context.Background()

	// Worker 1 applies cleanly (edit + create). Worker 2 then fails: its edit
	// targets text worker 1 already replaced — a stale-edit conflict.
	st1 := &editStage{}
	st1.add(stagedOp{kind: "edit", path: f, oldString: "beta", newString: "BETA", summary: "edit a.txt"})
	created := filepath.Join(dir, "created.txt")
	st1.add(stagedOp{kind: "write", path: created, content: "brand new", summary: "create created.txt"})
	st2 := &editStage{}
	st2.add(stagedOp{kind: "edit", path: f, oldString: "beta", newString: "x", summary: "stale edit"})

	out, err := m.applyPlannedOps(ctx, []peWorkerResult{
		{task: "update a.txt and add a file", stage: st1},
		{task: "conflicting update", stage: st2},
		{task: "planner blew up", stage: &editStage{}, err: errors.New("model unavailable")},
	})
	if err == nil {
		t.Fatalf("expected atomic abort error, got success: %q", out)
	}
	msg := err.Error()
	for _, want := range []string{"aborted", "rolled back", "CONFLICT", f, created} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got:\n%s", want, msg)
		}
	}

	// Rollback restored pre-batch content and removed the created file.
	if got, _ := os.ReadFile(f); string(got) != "alpha beta gamma" {
		t.Errorf("a.txt not rolled back, got %q", got)
	}
	if _, statErr := os.Stat(created); !os.IsNotExist(statErr) {
		t.Errorf("created.txt should have been removed by rollback (err=%v)", statErr)
	}

	// The turn snapshot must be untouched by the rollback: /undo still has a
	// record for the turn. It has nothing left to restore — the rollback
	// already put the workspace back — which is the point: the two mechanisms
	// don't consume each other.
	m.finalizeCheckpoint("parallel_edit rollback test")
	if msg, _ := m.undoLast(); !strings.HasPrefix(msg, "undid") {
		t.Errorf("expected /undo record to survive the rollback, got %q", msg)
	}
	if got, _ := os.ReadFile(f); string(got) != "alpha beta gamma" {
		t.Errorf("undo after rollback changed a.txt, got %q", got)
	}
}

// TestParallelEditApplySuccess locks in that a clean batch applies every op,
// reports each worker's applied changes, and touches no rollback machinery.
func TestParallelEditApplySuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(f1, []byte("one two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{tools: tools.DefaultRegistry()}

	st1 := &editStage{}
	st1.add(stagedOp{kind: "edit", path: f1, oldString: "two", newString: "2", summary: "edit a.txt"})
	st2 := &editStage{}
	st2.add(stagedOp{kind: "delete", path: f2, summary: "remove b.txt"})

	out, err := m.applyPlannedOps(context.Background(), []peWorkerResult{
		{task: "update a.txt", stage: st1},
		{task: "delete b.txt", stage: st2},
	})
	if err != nil {
		t.Fatalf("clean batch should succeed: %v", err)
	}
	if !strings.Contains(out, "2 change(s) applied") || strings.Contains(out, "CONFLICT") {
		t.Errorf("unexpected report: %q", out)
	}
	if got, _ := os.ReadFile(f1); string(got) != "one 2" {
		t.Errorf("a.txt not applied, got %q", got)
	}
	if _, statErr := os.Stat(f2); !os.IsNotExist(statErr) {
		t.Errorf("b.txt should be deleted (err=%v)", statErr)
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

	m := &Model{tools: tools.DefaultRegistry()}
	reg := m.plannerRegistry(&editStage{})
	for _, tdef := range reg.Definitions() {
		if !plannerAllowed(tdef.Function.Name) {
			t.Errorf("plannerRegistry exposed disallowed tool %q", tdef.Function.Name)
		}
	}
}
