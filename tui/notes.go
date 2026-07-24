package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

func (n *sessionNotes) load() {
	n.mu.Lock()
	defer n.mu.Unlock()
	data, err := os.ReadFile(notesFile)
	if err == nil {
		n.text = string(data)
	}
}

func (n *sessionNotes) save() {
	_ = os.WriteFile(notesFile, []byte(n.text), 0o644)
}

func (n *sessionNotes) get() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.text
}

func (n *sessionNotes) set(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.text = s
	n.save()
}

func (n *sessionNotes) appendLine(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.text != "" && !strings.HasSuffix(n.text, "\n") {
		n.text += "\n"
	}
	n.text += s
	n.save()
}

func readNotesTool(notes *sessionNotes) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "read_session_notes",
			Description: "Read your persistent session notes — a scratchpad you can use to record observations, decisions, and reminders that persist across the whole conversation. Useful for keeping context when your own context window is small.",
			Parameters:  tools.Schema{Type: "object", Properties: map[string]tools.Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			text := notes.get()
			if text == "" {
				return "(empty — use update_session_notes or append_session_notes to record info)", nil
			}
			return text, nil
		},
	}
}

func updateNotesTool(notes *sessionNotes) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "update_session_notes",
			Description: "Replace your session notes scratchpad with a new value. Use append_session_notes to add to it instead.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"content": {Type: "string", Description: "Full new content of the notes."},
				},
				Required: []string{"content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			notes.set(a.Content)
			return fmt.Sprintf("notes updated (%d chars)", len(a.Content)), nil
		},
	}
}

const notesFile = ".ollama_notes.md"

type sessionNotes struct {
	mu   sync.Mutex
	text string
}
