package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/javanhut/ollama_code/internal/repomap"
)

// RepoMapTool exposes internal/repomap: a ranked outline of the workspace's
// most-referenced files and their top-level definitions, fit to a token
// budget. One call replaces the several list/grep/read rounds a small model
// otherwise spends rediscovering a codebase's layout.
func RepoMapTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "repo_map",
			Description: "Outline the repository: the most important files (ranked by how much other code references them) with their top-level functions, types and classes and line numbers. Use it first to orient yourself in an unfamiliar codebase, then read_file the relevant files. Pass focus paths to rank what those files depend on first.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"focus":      {Type: "array", Description: "Optional workspace-relative file paths to center the ranking on (e.g. files you are about to change).", Items: &Property{Type: "string"}},
					"max_tokens": {Type: "number", Description: "Approximate size budget for the outline (default 1500, max 6000)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Focus     []string `json:"focus"`
				MaxTokens int      `json:"max_tokens"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			if a.MaxTokens <= 0 {
				a.MaxTokens = 1500
			}
			a.MaxTokens = min(a.MaxTokens, 6000)
			root := WorkspaceRoot()
			focus := make([]string, 0, len(a.Focus))
			for _, p := range a.Focus {
				if err := jailCheck(p); err != nil {
					return "", err
				}
				abs := resolveJailPath(p)
				if rel, err := filepath.Rel(resolveJailPath(root), abs); err == nil {
					focus = append(focus, rel)
				}
			}
			files, err := repomap.Build(root, repomap.Options{Focus: focus})
			if err != nil {
				return "", err
			}
			out := repomap.Render(files, a.MaxTokens)
			if out == "" {
				return "no source files with recognizable definitions found (supported: Go, Python, JS/TS, Rust, Java, Ruby)", nil
			}
			return out, nil
		},
	}
}
