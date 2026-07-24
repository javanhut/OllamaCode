package tui

import (
	"context"
	"strings"
	"testing"
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
