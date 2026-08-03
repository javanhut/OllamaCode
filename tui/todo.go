package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

type todoStatus string

const (
	todoPending    todoStatus = "pending"
	todoInProgress todoStatus = "in_progress"
	todoCompleted  todoStatus = "completed"
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

func (t *todoList) set(items []todoItem) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = items
}

func (t *todoList) get() []todoItem {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]todoItem(nil), t.items...)
}

// openCount returns how many items are not yet completed.
func (t *todoList) openCount() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, it := range t.items {
		if it.Status != todoCompleted {
			n++
		}
	}
	return n
}

// openSummary lists the not-yet-completed items, one per line, for the
// keep-going nudge.
func (t *todoList) openSummary() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, it := range t.items {
		if it.Status != todoCompleted {
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
			Description: "Maintain a checklist for a multi-step task. Pass the FULL list each call — it replaces the previous one. Mark exactly ONE item \"in_progress\" while you work it, and flip it to \"completed\" the moment it's done, then start the next. Use this for any task with 3+ steps so progress is visible and nothing is dropped. Keep items short and concrete. Do NOT end your turn while items remain incomplete unless you're blocked.",
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
								"status":  {Type: "string", Enum: []string{"pending", "in_progress", "completed"}, Description: "pending | in_progress | completed"},
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
				case todoPending, todoInProgress, todoCompleted:
				default:
					a.Todos[i].Status = todoPending
				}
				if a.Todos[i].Status == todoCompleted {
					done++
				}
			}
			list.set(a.Todos)
			return fmt.Sprintf("todo list updated: %d/%d completed", done, len(a.Todos)), nil
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
