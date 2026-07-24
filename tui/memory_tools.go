package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/javanhut/ollama_code/internal/memory"
	"github.com/javanhut/ollama_code/tools"
)

func appendNotesTool(notes *sessionNotes) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "append_session_notes",
			Description: "Append a line to your repository-specific session notes. Use this for project state and hashes.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"content": {Type: "string", Description: "Text to append."},
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
			notes.appendLine(a.Content)
			return fmt.Sprintf("appended %d chars to notes", len(a.Content)), nil
		},
	}
}

// rememberTool stores a fact. Defaults to short-term (session-only); set
// persist=true to write to long-term memory across sessions. Tool calls are
// invisible in the transcript — acknowledge in your reply instead.
func rememberTool(mem *memory.Store) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "remember",
			Description: "Store a fact in memory. Use persist=true when the user says 'remember' explicitly, or when you decide a detail (preferences, decisions, identity) is worth carrying across future sessions. Use persist=false for observations that only matter for the rest of this conversation. The user does NOT see this tool call — always acknowledge in your reply that you've stored it.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"content": {Type: "string", Description: "The fact to remember, in a self-contained sentence the future you can read cold."},
					"persist": {Type: "boolean", Description: "true = long-term (persists across sessions); false = short-term (session only). Default false."},
				},
				Required: []string{"content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Content string `json:"content"`
				Persist bool   `json:"persist"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("content is required")
			}
			if _, err := mem.Remember(a.Content, a.Persist); err != nil {
				return "", err
			}
			tier := "short-term (session)"
			if a.Persist {
				tier = "long-term (persistent)"
			}
			return fmt.Sprintf("stored in %s memory", tier), nil
		},
	}
}

// recallTool returns memories matching a query. Empty query returns everything.
func recallTool(mem *memory.Store) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "recall",
			Description: "Search memory for matching entries across both short-term (session) and long-term (persistent) tiers. Empty query returns all memories. The user does NOT see this tool call.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"query": {Type: "string", Description: "Optional substring to filter entries (case-insensitive). Omit or leave empty to return everything."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			st, lt := mem.Recall(a.Query)
			if len(st) == 0 && len(lt) == 0 {
				return "(no matching memories)", nil
			}
			var b strings.Builder
			if len(lt) > 0 {
				b.WriteString("LONG-TERM:\n")
				b.WriteString(memory.FormatEntries(lt))
				b.WriteString("\n")
			}
			if len(st) > 0 {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("SHORT-TERM:\n")
				b.WriteString(memory.FormatEntries(st))
			}
			return b.String(), nil
		},
	}
}

// forgetTool deletes entries matching a query.
func forgetTool(mem *memory.Store) tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "forget",
			Description: "Delete all memory entries (short-term and long-term) whose content matches the query substring (case-insensitive). Use only when the user explicitly asks to forget something. The user does NOT see this tool call — confirm in your reply.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"query": {Type: "string", Description: "Substring identifying the memories to remove."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			n, err := mem.Forget(a.Query)
			if err != nil {
				return "", err
			}
			if n == 0 {
				return "no matching memories to forget", nil
			}
			return fmt.Sprintf("forgot %d memory entry/entries", n), nil
		},
	}
}

// leanToolNames is the core toolset sent to small models. Sending 60+ JSON
// schemas to a sub-15B model wastes thousands of prompt tokens and wrecks its
// tool-selection accuracy; a focused set covers the whole edit workflow.
