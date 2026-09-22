package tui

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

// repoSnapshotBudget bounds the whole snapshot, so a slow VCS or a huge tree
// delays the first request by at most this much.
const repoSnapshotBudget = 3 * time.Second

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// repoSnapshotBlock is a one-time picture of the repository: VCS status, the
// last few commits, and a shallow file tree. Without it every session opens
// with the model spending its first several tool calls rediscovering the same
// layout, which on a local model is the slowest part of a turn. It is taken
// once and cached, so it stays in the KV-cached system prefix; the text says
// it is a snapshot so the model re-checks before relying on it.
//
// Small models get status and commits but no tree, to keep the compact prompt
// compact. repo_snapshot=false in config turns it off.
func (m *Model) repoSnapshotBlock() string {
	if m.cfg.RepoSnapshot != nil && !*m.cfg.RepoSnapshot {
		return ""
	}
	if m.repoSnapshotDone {
		return m.repoSnapshot
	}
	m.repoSnapshotDone = true
	if m.tools == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), repoSnapshotBudget)
	defer cancel()
	small := m.profile.smallModel()

	statusLines, logCount := 40, 5
	if small {
		statusLines, logCount = 15, 3
	}
	var sections []string
	if out := m.snapshotTool(ctx, "git_status", nil); out != "" {
		sections = append(sections, "## Status\n"+capLines(out, statusLines))
	}
	if out := m.snapshotTool(ctx, "git_log", map[string]any{"count": logCount}); out != "" {
		sections = append(sections, "## Recent commits\n"+capLines(out, logCount*6))
	}
	if !small {
		if out := m.snapshotTool(ctx, "get_project_tree", map[string]any{"max_depth": 2, "max_entries": 80}); out != "" {
			sections = append(sections, "## Files (depth 2)\n"+out)
		}
	}
	if len(sections) == 0 {
		return ""
	}
	m.repoSnapshot = "\n# Repository snapshot\nTaken when this session started; it does NOT update. Re-run git_status or list_directory before relying on it after changes.\n\n" +
		strings.Join(sections, "\n\n") + "\n"
	return m.repoSnapshot
}

// snapshotTool runs a read tool for the snapshot and returns its plain
// output, or "" if it failed or isn't registered.
func (m *Model) snapshotTool(ctx context.Context, name string, args map[string]any) string {
	if ctx.Err() != nil {
		return ""
	}
	raw := json.RawMessage("{}")
	if args != nil {
		raw, _ = json.Marshal(args)
	}
	out, err := m.tools.Invoke(ctx, tools.ToolCall{Function: tools.ToolCallFunction{Name: name, Arguments: raw}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ansiEscape.ReplaceAllString(out, ""))
}

func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n... (truncated; run git_status for the rest)"
}
