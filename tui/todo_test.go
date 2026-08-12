package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

func TestTodoWriteTool(t *testing.T) {
	list := &todoList{}
	tool := todoWriteTool(list)
	args := []byte(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"},{"content":"c","status":"pending"}]}`)
	out, err := tool.Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1/3 completed") {
		t.Fatalf("summary = %q", out)
	}
	if list.openCount() != 2 {
		t.Fatalf("openCount = %d, want 2", list.openCount())
	}
	if items := list.get(); len(items) != 3 || items[1].Status != todoInProgress {
		t.Fatalf("items = %+v", items)
	}

	// An unknown status normalizes to pending (still counts as open).
	if _, err := tool.Handler(context.Background(), []byte(`{"todos":[{"content":"x","status":"bogus"}]}`)); err != nil {
		t.Fatal(err)
	}
	if got := list.get()[0].Status; got != todoPending {
		t.Fatalf("bad status normalized to %q, want pending", got)
	}
	if list.openCount() != 1 {
		t.Fatalf("openCount = %d, want 1", list.openCount())
	}
}

func TestTodoReadTool(t *testing.T) {
	list := &todoList{}
	read := todoReadTool(list)

	// An empty list reads back as an empty array, not null, so the output is
	// always valid todo_write input.
	out, err := read.Handler(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"todos":[]}` {
		t.Fatalf("empty read = %q", out)
	}

	write := todoWriteTool(list)
	if _, err := write.Handler(context.Background(), []byte(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, err = read.Handler(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var round struct {
		Todos []todoItem `json:"todos"`
	}
	if err := json.Unmarshal([]byte(out), &round); err != nil {
		t.Fatalf("read output not in todo_write's shape: %v", err)
	}
	if len(round.Todos) != 2 || round.Todos[1].Status != todoInProgress {
		t.Fatalf("read items = %+v", round.Todos)
	}

	// Read-modify-write: the read output must feed straight back into
	// todo_write. Flip a status first — an unmodified rewrite is refused on
	// purpose (TestTodoWriteRejectsUnchangedList).
	modified := strings.Replace(out, `"in_progress"`, `"completed"`, 1)
	if _, err := write.Handler(context.Background(), []byte(modified)); err != nil {
		t.Fatalf("read output must be valid todo_write input: %v", err)
	}
	if items := list.get(); len(items) != 2 || items[0].Content != "a" || items[1].Status != todoCompleted {
		t.Fatalf("round-trip items = %+v", items)
	}
}

// A byte-identical rewrite must fail. The success receipt for a no-op is what
// let a model re-send the same checklist for dozens of rounds while the loop
// guard counted each one as work.
func TestTodoWriteRejectsUnchangedList(t *testing.T) {
	list := &todoList{}
	write := todoWriteTool(list)
	args := []byte(`{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"pending"}]}`)
	if _, err := write.Handler(context.Background(), args); err != nil {
		t.Fatalf("first write: %v", err)
	}
	out, err := write.Handler(context.Background(), args)
	if err == nil {
		t.Fatalf("identical rewrite reported success: %q", out)
	}
	if !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("error must tell the model nothing changed: %v", err)
	}
	if items := list.get(); len(items) != 2 || items[0].Status != todoInProgress {
		t.Fatalf("refused rewrite still mutated the list: %+v", items)
	}

	// A status flip on the same items is real progress and must still write.
	if _, err := write.Handler(context.Background(), []byte(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`)); err != nil {
		t.Fatalf("changed list rejected: %v", err)
	}
	if items := list.get(); items[0].Status != todoCompleted {
		t.Fatalf("items = %+v", items)
	}
}

func TestReconcileTodosAtSuccessfulTurnEnd(t *testing.T) {
	m := &Model{
		mode:          WriteMode,
		maxSteps:      40,
		autoContinues: maxAutoContinues,
		todos:         &todoList{},
		history:       []api.Message{{Role: "assistant", Content: "Implemented and verified the fix."}},
	}
	m.todos.set([]todoItem{
		{Content: "implement fix", Status: todoInProgress},
		{Content: "run tests", Status: todoPending},
	})

	if changed := m.reconcileTodosAtTurnEnd(); changed != 2 {
		t.Fatalf("reconciled %d todos, want 2", changed)
	}
	if open := m.todos.openCount(); open != 0 {
		t.Fatalf("successful turn left %d todos open", open)
	}
}

func TestReconcileTodosPreservesBlockedOrBudgetStoppedWork(t *testing.T) {
	newModel := func(answer string) *Model {
		m := &Model{
			mode:          WriteMode,
			maxSteps:      40,
			autoContinues: maxAutoContinues,
			todos:         &todoList{},
			history:       []api.Message{{Role: "assistant", Content: answer}},
		}
		m.todos.set([]todoItem{{Content: "finish work", Status: todoInProgress}})
		return m
	}

	blocked := newModel("I am blocked by missing credentials.")
	if changed := blocked.reconcileTodosAtTurnEnd(); changed != 0 || blocked.todos.openCount() != 1 {
		t.Fatal("explicitly blocked work was marked completed")
	}

	budgetStopped := newModel("Work stopped at the step limit.")
	budgetStopped.stepCount = budgetStopped.turnStepLimit()
	if changed := budgetStopped.reconcileTodosAtTurnEnd(); changed != 0 || budgetStopped.todos.openCount() != 1 {
		t.Fatal("budget-stopped work was marked completed")
	}
}
