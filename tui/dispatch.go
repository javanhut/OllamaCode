package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/safeshell"
	"github.com/javanhut/ollama_code/tools"
)

var diffPreviewTools = map[string]bool{
	"write_file": true, "edit_file": true, "apply_diff": true, "append_file": true,
}

type pendingBatch struct {
	calls    []tools.ToolCall
	results  []api.Message
	started  []bool
	done     int
	index    int
	allowAll bool
	preview  string
	gen      int // turn generation this batch belongs to
}

func (m *Model) invokeTool(ctx context.Context, call tools.ToolCall) api.Message {
	m.logActivity("Tool: " + call.Function.Name)
	// Best-effort repair of almost-valid JSON arguments before dispatch.
	call.Function.Arguments = tools.SalvageJSON(call.Function.Arguments)
	// Checkpoint affected files before a mutating tool runs, so /undo can revert.
	if paths := tools.MutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
		m.snapshotBeforeMutate(paths)
	}
	result, err := m.tools.Invoke(ctx, call)
	// Last-resort escalation: if the failure is an argument problem, ask the
	// model for schema-valid arguments via constrained decoding and retry once.
	if err != nil && tools.ShouldFormatRepair(call, err) {
		if fixed, ok := m.repairArgsViaFormat(call); ok {
			call.Function.Arguments = fixed
			result, err = m.tools.Invoke(ctx, call)
		}
	}
	if err != nil {
		result = tools.RepairHint(call, err)
	}
	return api.Message{
		Role:     "tool",
		ToolName: call.Function.Name,
		Content:  result,
	}
}

func (m *Model) invokeToolCmd(gen, index int, call tools.ToolCall) tea.Cmd {
	return func() tea.Msg {
		var req *modeSwitchRequest
		if call.Function.Name == "switch_mode" {
			req, _ = parseModeSwitchArgs(call.Function.Arguments)
		}

		timeout := toolCallTimeout(call)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		done := make(chan api.Message, 1)
		go func() {
			// A panic in a tool handler must not tear down the whole program;
			// convert it into an error result so the turn can recover.
			defer func() {
				if r := recover(); r != nil {
					done <- api.Message{
						Role:     "tool",
						ToolName: call.Function.Name,
						Content:  fmt.Sprintf("error: tool %q panicked: %v", call.Function.Name, r),
					}
				}
			}()
			done <- m.invokeTool(ctx, call)
		}()

		select {
		case result := <-done:
			return toolResultMsg{gen: gen, index: index, result: result, modeSwitch: req}
		case <-ctx.Done():
			return toolResultMsg{
				gen:   gen,
				index: index,
				result: api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: tool %q timed out after %s. Treat this as a stuck call: do not retry the same arguments blindly; inspect state, use a narrower command or shorter timeout, and continue with another approach.", call.Function.Name, timeout),
				},
				modeSwitch: nil,
			}
		}
	}
}

func toolCallTimeout(call tools.ToolCall) time.Duration {
	switch call.Function.Name {
	case "run_shell":
		return shellToolCallTimeout(call)
	case "git_status", "git_diff", "git_log", "git_show", "git_branch",
		"read_file", "list_directory", "find_files", "grep", "find_symbol", "file_info",
		"get_working_directory", "process_list", "disk_usage", "system_info",
		"read_session_notes", "recall":
		return localInspectToolTimeout
	case "write_file", "edit_file", "append_file", "apply_diff", "delete_file", "move_file",
		"copy_file", "make_directory", "touch", "git_add", "git_commit", "git_checkout",
		"git_stash", "git_merge", "git_reset", "git_remote", "process_kill",
		"update_session_notes", "append_session_notes", "remember", "forget":
		return localMutatingToolTimeout
	case "web_search", "web_fetch", "code_index", "semantic_search":
		return networkToolTimeout
	case "spawn_subagent", "parallel_edit":
		return longRunningToolTimeout
	default:
		return defaultToolCallTimeout
	}
}

func shellToolCallTimeout(call tools.ToolCall) time.Duration {
	var a struct {
		TimeoutSec float64 `json:"timeout_sec"`
	}
	_ = json.Unmarshal(call.Function.Arguments, &a)
	timeout := 30 * time.Second
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec * float64(time.Second))
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	return timeout + shellToolTimeoutGrace
}

func (m *Model) processPendingTools() tea.Cmd {
	if m.pending == nil {
		return nil
	}

	if m.pending.done >= len(m.pending.calls) {
		batchCalls := m.pending.calls
		m.history = append(m.history, m.pending.results...)
		m.pending = nil

		// No-progress nudge: the model is alternating between the same two
		// actions. Tell it once, rather than letting it spin.
		if !m.oscillationWarned && tools.IsOscillating(m.recentCalls) {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[NO PROGRESS DETECTED] You are alternating between the same actions without making progress. Stop, state your blocker explicitly, and try a different approach.",
			})
			m.oscillationWarned = true
		}

		// Inspection calls include arguments in their repeat identity, so reading
		// different files or running different searches is progress. Mutation and
		// control tools remain name-based to catch varied-argument spam.
		batchTool, warnRepeat, stopRepeat, announceStop := m.observeRepeatedBatch(batchCalls)
		if warnRepeat {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: fmt.Sprintf("[REPEATING ACTION] You have called %q %d times in a row without making progress. Stop repeating it — take a different action, or if you're blocked, explain the blocker to the user in plain text.", batchTool, m.sameToolStreak),
			})
		}
		if announceStop {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: fmt.Sprintf("[LOOP BROKEN] You called %q %d times in a row. Tools are disabled for your next message — respond to the user in plain text only.", batchTool, m.sameToolStreak),
			})
		}
		if stopRepeat {
			m.suppressToolsOnce = true
		}

		// Step budget: cap tool-call rounds per user turn so a confused model
		// can't loop forever burning tokens.
		m.stepCount++
		limit := m.maxSteps
		if m.mode == AutoMode {
			limit = 100
		}
		if m.stepCount >= limit {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[STEP BUDGET EXHAUSTED] You have used your tool-call budget for this turn. Stop calling tools: summarize what you did, what remains, and ask the user how to proceed.",
			})
			m.suppressToolsOnce = true
		}

		cmd := m.startStream()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return cmd
	}

	var cmds []tea.Cmd
	for i, call := range m.pending.calls {
		if m.pending.started[i] {
			continue
		}

		if !m.toolAllowedInMode(call.Function.Name) {
			m.failedCalls[tools.CallFingerprint(call)]++
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: tool %q not allowed in %s mode (press tab to switch modes)", call.Function.Name, m.mode),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		if call.Function.Name == "run_shell" {
			cmd := safeshell.ExtractShellCommand(call.Function.Arguments)

			// Explore-mode read-only allowlist (per-segment bin/sub check).
			if m.mode == ExploreMode {
				if ok, reason := safeshell.IsExploreReadOnlyShell(cmd); !ok {
					m.failedCalls[tools.CallFingerprint(call)]++
					m.pending.results[i] = api.Message{
						Role:     "tool",
						ToolName: call.Function.Name,
						Content:  fmt.Sprintf("error: %s. Call switch_mode(\"plan\", ...) and then switch_mode(\"write\", ...) to run mutating commands.", reason),
					}
					m.pending.started[i] = true
					m.pending.done++
					continue
				}
			}

			// VCS bypass guard (all modes): in an ivaldi repo, reject bare
			// `git` invocations that would bypass the MCP translation layer
			// and fail with "not a git repository". The git_* tools translate
			// transparently; raw `git` via run_shell does not.
			if ok, reason := safeshell.InterceptVCSBypass(cmd, tools.DetectVCS()); !ok {
				m.failedCalls[tools.CallFingerprint(call)]++
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  "error: " + reason,
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
		}

		if call.Function.Name == "switch_mode" {
			req, err := parseModeSwitchArgs(call.Function.Arguments)
			switch {
			case err != nil:
				// Genuinely malformed (bad/unknown mode) — report and move on.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: %v", err),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case req.target == AutoMode:
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  "error: transition to 'auto' mode can only be triggered by the user explicitly, not via tool call.",
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case req.target == m.mode:
				// Redundant switch: succeed as a no-op rather than erroring, so a
				// confused model doesn't spin retrying the same switch.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("already in %s mode", m.mode),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
			// Any real transition (forward or backward) is allowed; it's applied
			// when the result returns (toolResultMsg -> applyModeTransition).
			// Backward switches go to a safer/more-restrictive mode; permission
			// prompts still gate destructive tools in write mode.
		}

		// Short-circuit a call that has already failed identically: re-running
		// it won't help and just burns a round-trip.
		fp := tools.CallFingerprint(call)
		if m.failedCalls[fp] >= maxSameCallFailures {
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: you already called %q with these exact arguments %d times and it failed each time. Do not repeat it — change the arguments or use a different approach.", call.Function.Name, m.failedCalls[fp]),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		// Explore-mode run_shell calls are prechecked above and are read-only,
		// so they don't need a permission prompt.
		exploreReadOnly := m.mode == ExploreMode && call.Function.Name == "run_shell"
		if m.shouldPromptPermission(call) && !exploreReadOnly {
			m.pending.index = i
			m.pending.preview = computePreview(call)
			m.state = statePermission
			m.refreshTranscript()
			break
		}

		m.recentCalls = append(m.recentCalls, fp)
		if len(m.recentCalls) > recentCallsKept {
			m.recentCalls = m.recentCalls[len(m.recentCalls)-recentCallsKept:]
		}
		m.pending.started[i] = true
		cmds = append(cmds, m.invokeToolCmd(m.pending.gen, i, call))
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	// Mode/preflight failures above complete synchronously and therefore do not
	// produce a toolResultMsg to re-enter this method. Finalize the batch now;
	// otherwise a batch made entirely of rejected calls remains stuck forever
	// at TOOLS n/n.
	if m.pending != nil && m.pending.done >= len(m.pending.calls) {
		return m.processPendingTools()
	}

	return nil
}

func computePreview(call tools.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Function.Arguments, &args)

	switch call.Function.Name {
	case "switch_mode":
		mode, _ := args["mode"].(string)
		reason, _ := args["reason"].(string)
		return fmt.Sprintf("Switch mode to: %s\nReason: %s", strings.TrimSpace(mode), strings.TrimSpace(reason))
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		old, err := os.ReadFile(path)
		if err != nil {
			return "(new file " + path + ")\n" + addedLines(truncatePreview(content, 20))
		}
		return simpleDiff(string(old), content, 10)
	case "edit_file":
		path, _ := args["path"].(string)
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		return path + "\n" + simpleDiff(oldStr, newStr, 3)
	case "apply_diff":
		path, _ := args["path"].(string)
		search, _ := args["search"].(string)
		replace, _ := args["replace"].(string)
		return path + "\n" + simpleDiff(search, replace, 3)
	case "append_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		label := path + " — appended:"
		if _, err := os.Stat(path); err != nil {
			label = "(new file " + path + ") — appended:"
		}
		return label + "\n" + addedLines(truncatePreview(content, 15))
	case "delete_file":
		path, _ := args["path"].(string)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Sprintf("%s (not found)", path)
		}
		var preview strings.Builder
		preview.WriteString(fmt.Sprintf("%s (%d bytes)\n", path, info.Size()))
		if !info.IsDir() {
			data, _ := os.ReadFile(path)
			lines := strings.Split(string(data), "\n")
			for i := 0; i < 3 && i < len(lines); i++ {
				preview.WriteString(lines[i] + "\n")
			}
			if len(lines) > 3 {
				preview.WriteString("...")
			}
		}
		return preview.String()
	case "move_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("move %s → %s", src, dst)
	case "copy_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("copy %s → %s", src, dst)
	case "run_shell":
		cmd, _ := args["command"].(string)
		return fmt.Sprintf("shell: %s", cmd)
	case "git_add":
		paths, _ := args["paths"].(string)
		return fmt.Sprintf("git add %s", paths)
	case "git_commit":
		msg, _ := args["message"].(string)
		return fmt.Sprintf("git commit -m %q", msg)
	default:
		return ""
	}
}

// addedLines prefixes each line with '+' so a preview of new/appended content
// colorizes as additions in the permission modal.
func addedLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "+" + l
	}
	return strings.Join(lines, "\n")
}

func truncatePreview(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n..."
}

func simpleDiff(old, new string, context int) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")
	// Very naive diff: find first changed line and last changed line
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	endOld := len(oldLines) - 1
	endNew := len(newLines) - 1
	for endOld >= start && endNew >= start && oldLines[endOld] == newLines[endNew] {
		endOld--
		endNew--
	}
	ctxStart := max(start-context, 0)
	ctxEndOld := endOld + context
	if ctxEndOld >= len(oldLines) {
		ctxEndOld = len(oldLines) - 1
	}
	ctxEndNew := endNew + context
	if ctxEndNew >= len(newLines) {
		ctxEndNew = len(newLines) - 1
	}
	var b strings.Builder
	if ctxStart > 0 {
		b.WriteString("...\n")
	}
	for i := ctxStart; i <= ctxEndOld && i < len(oldLines); i++ {
		if i >= start && i <= endOld {
			fmt.Fprintf(&b, "-%s\n", oldLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", oldLines[i])
		}
	}
	for i := ctxStart; i <= ctxEndNew && i < len(newLines); i++ {
		if i >= start && i <= endNew {
			fmt.Fprintf(&b, "+%s\n", newLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", newLines[i])
		}
	}
	if ctxEndNew < len(newLines)-1 || ctxEndOld < len(oldLines)-1 {
		b.WriteString("...")
	}
	return b.String()
}

func (m *Model) shouldPromptPermission(call tools.ToolCall) bool {
	if m.pending.allowAll {
		return false
	}
	if !destructiveToolNames[call.Function.Name] {
		return false
	}
	if m.mode == AutoMode {
		var args map[string]any
		if err := json.Unmarshal(call.Function.Arguments, &args); err == nil {
			for _, key := range []string{"path", "dest", "destination", "new_path", "to", "source", "src", "working_dir"} {
				if p, ok := args[key].(string); ok && p != "" {
					if !m.isPathInTrustedFolder(p) {
						return true
					}
				}
			}
		}
		return false
	}
	// For all other modes (Explore, Plan, Write), prompt for all destructive tools
	return true
}

func (m *Model) isPathInTrustedFolder(targetPath string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	var absTarget string
	if filepath.IsAbs(targetPath) {
		absTarget = filepath.Clean(targetPath)
	} else {
		absTarget = filepath.Clean(filepath.Join(absCwd, targetPath))
	}
	rel, err := filepath.Rel(absCwd, absTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
