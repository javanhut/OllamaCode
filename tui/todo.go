package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

type todoStatus string

const (
	todoPending    todoStatus = "pending"
	todoInProgress todoStatus = "in_progress"
	todoCompleted  todoStatus = "completed"
	// todoBlocked is the model SAYING it is stuck, rather than the harness
	// inferring it from prose. responseReportsBlocker below still reads the
	// assistant's text for models that never set this, but that is a phrase list
	// against free-form English: it misses "I've run out of options here" and
	// trips on "the user said they cannot complete the migration". A status the
	// model sets is the signal that actually means what it says.
	todoBlocked todoStatus = "blocked"
)

type todoItem struct {
	Content string     `json:"content"`
	Status  todoStatus `json:"status"`
}

// todoList is the model's checklist for the current multi-step task. It's
// mutated from the tool goroutine and read by the render/loop, so it's guarded.
type todoList struct {
	mu    sync.Mutex
	items []todoItem
}

// set replaces the checklist and reports whether it actually changed. The
// comparison happens under the same lock as the store, so a second tool
// goroutine cannot slip a write in between and make a real edit look like a
// no-op. Callers that only seed the list ignore the result.
func (t *todoList) set(items []todoItem) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if slices.Equal(t.items, items) {
		return false
	}
	t.items = items
	return true
}

func (t *todoList) get() []todoItem {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]todoItem(nil), t.items...)
}

// openCount returns how many items are still actionable. A blocked item is not
// completed, but it is also not something the model can be nudged into doing —
// counting it here is what would make the [CONTINUE] nudge keep demanding work
// on the one item the model already said it cannot do.
func (t *todoList) openCount() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, it := range t.items {
		if it.Status != todoCompleted && it.Status != todoBlocked {
			n++
		}
	}
	return n
}

// hasBlocked reports whether the model has declared any item blocked.
func (t *todoList) hasBlocked() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, it := range t.items {
		if it.Status == todoBlocked {
			return true
		}
	}
	return false
}

// completeOpen closes stale checklist entries after the model has repeatedly
// reported completion without issuing the final todo_write update. The caller
// decides whether the turn genuinely reached a successful terminal state.
func (t *todoList) completeOpen() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := 0
	for i := range t.items {
		if t.items[i].Status != todoCompleted {
			t.items[i].Status = todoCompleted
			changed++
		}
	}
	return changed
}

// reconcileTodosAtTurnEnd is the final safety net for weak models that ignore
// the todo_write reminder. We only close entries after all reminder attempts,
// while still under the step budget, and never for an explicitly blocked or
// verification-failed turn.
func (m *Model) reconcileTodosAtTurnEnd() int {
	if m.todos == nil || m.todos.openCount() == 0 || m.autoContinues < maxAutoContinues {
		return 0
	}
	limit := m.turnStepLimit()
	if m.mode == AutoMode {
		limit = autoModeMaxSteps
	}
	if m.stepCount >= limit || (m.verifyAttempts >= maxVerifyAttempts && m.lastVerification == "") {
		return 0
	}
	// Either signal stops the force-complete, and the conservative direction is
	// clear: a false positive leaves stale items open in the sidebar, while a
	// false negative writes "completed" onto work that never happened.
	if m.todos.hasBlocked() || responseReportsBlocker(m.latestAssistantContent()) {
		return 0
	}
	return m.todos.completeOpen()
}

func (m *Model) latestAssistantContent() string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "assistant" && strings.TrimSpace(m.history[i].Content) != "" {
			return m.history[i].Content
		}
	}
	return ""
}

func responseReportsBlocker(s string) bool {
	s = strings.ToLower(s)
	for _, marker := range []string{
		"[blocked]", "i am blocked", "i'm blocked", "blocked by",
		"unable to complete", "cannot complete", "can't complete", "could not complete", "couldn't complete",
		"unable to finish", "cannot finish", "can't finish", "could not finish", "couldn't finish",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// openSummary lists the still-actionable items, one per line, for the
// keep-going nudge. Blocked items are left out for the same reason openCount
// skips them: the nudge says "take the next item now", and the next item must
// not be the one the model already reported it cannot do.
func (t *todoList) openSummary() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, it := range t.items {
		if it.Status != todoCompleted && it.Status != todoBlocked {
			fmt.Fprintf(&b, "- [%s] %s\n", it.Status, it.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// todoWriteTool lets the model maintain a task checklist. The full list is passed
// each call and replaces the previous one (like Claude Code's TodoWrite).
func todoWriteTool(list *todoList) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "todo_write",
			Description: "Maintain a checklist for a multi-step task. Pass the FULL list each call — it replaces the previous one. Mark exactly ONE item \"in_progress\" while you work it, and flip it to \"completed\" the moment it's done, then start the next. Use this for any task with 3+ steps so progress is visible and nothing is dropped. Keep items short and concrete. Do NOT end your turn while items remain incomplete. If you genuinely cannot proceed on an item, set its status to \"blocked\" rather than leaving it pending or claiming it is done.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"todos": {
						Type:        "array",
						Description: "The complete todo list, in order.",
						Items: &tools.Property{
							Type: "object",
							Properties: map[string]tools.Property{
								"content": {Type: "string", Description: "Short, concrete description of the step."},
								"status":  {Type: "string", Enum: []string{"pending", "in_progress", "completed", "blocked"}, Description: "pending | in_progress | completed | blocked (you tried and cannot proceed — say why in your reply)"},
							},
							Required: []string{"content", "status"},
						},
					},
				},
				Required: []string{"todos"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Todos []todoItem `json:"todos"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			done := 0
			for i := range a.Todos {
				switch a.Todos[i].Status {
				case todoPending, todoInProgress, todoCompleted, todoBlocked:
				default:
					a.Todos[i].Status = todoPending
				}
				if a.Todos[i].Status == todoCompleted {
					done++
				}
			}
			// A rewrite that changes nothing is not a step forward, and answering
			// it with a success receipt is what sustains rewrite loops: the model
			// re-sends the same list, reads "todo list updated", and books it as
			// progress. Failing makes it ok:false, so the repeated-failure
			// short-circuit and the round-progress guard both see it for what it is.
			if !list.set(a.Todos) {
				return "", fmt.Errorf("todo list unchanged: every item already has this exact content and status, so nothing was written. Do not send this list again and do not reword items to force a write — take the next real action (read, edit, run a command) or give your final answer")
			}
			return fmt.Sprintf("todo list updated: %d/%d completed", done, len(a.Todos)), nil
		},
	}
}

// todoReadTool lets the model read its checklist back. todo_write replaces the
// whole list, so a model that can't read first has to rewrite blind and
// clobbers items it forgot. Returns JSON in exactly the shape todo_write
// accepts, making read-modify-write round-trips trivial. Read-only.
func todoReadTool(list *todoList) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "todo_read",
			Description: "Read the current todo checklist. Returns JSON {\"todos\":[{\"content\",\"status\"},...]} — the same shape todo_write accepts.",
			Parameters:  tools.Schema{Type: "object", Properties: map[string]tools.Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			items := list.get()
			if items == nil {
				items = []todoItem{} // emit [], not null, for a clean round-trip
			}
			out, err := json.Marshal(struct {
				Todos []todoItem `json:"todos"`
			}{Todos: items})
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

// todoSidebar renders the checklist as a sidebar block, one line per step.
// Empty when there are no todos.
func (m *Model) todoSidebar(inner int) string {
	items := m.todos.get()
	if len(items) == 0 {
		return ""
	}
	done := 0
	for _, it := range items {
		if it.Status == todoCompleted {
			done++
		}
	}
	var b strings.Builder
	b.WriteString(m.sidebarHeading(fmt.Sprintf("Tasks (%d/%d)", done, len(items))))
	for _, it := range items {
		text := truncatePlain(it.Content, inner-2)
		b.WriteString("\n")
		switch it.Status {
		case todoCompleted:
			b.WriteString(mutedStyle.Render("✔ " + text))
		case todoInProgress:
			b.WriteString(bodyStyle.Bold(true).Render("▶ " + text))
		default:
			b.WriteString(bodyStyle.Render("☐ " + text))
		}
	}
	return b.String()
}
