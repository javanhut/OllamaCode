package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

type Mode int

const (
	ExploreMode Mode = iota
	PlanMode
	WriteMode
	AutoMode
)

func (m Mode) String() string {
	switch m {
	case ExploreMode:
		return "explore"
	case PlanMode:
		return "plan"
	case WriteMode:
		return "write"
	case AutoMode:
		return "auto"
	}
	return "?"
}

func (m Mode) hint() string {
	switch m {
	case ExploreMode:
		return "read-only"
	case PlanMode:
		return "read + notes"
	case WriteMode:
		return "writes need approval"
	case AutoMode:
		return "autonomous (unlimited changes in workspace)"
	}
	return ""
}

func (m Mode) next() Mode {
	switch m {
	case ExploreMode:
		return PlanMode
	case PlanMode:
		return WriteMode
	default:
		return ExploreMode
	}
}

func (m Mode) color() color.Color {
	switch m {
	case ExploreMode:
		return lipgloss.Color("39") // Blue
	case PlanMode:
		return lipgloss.Color("220") // Yellow
	case WriteMode:
		return lipgloss.Color("196") // Red
	case AutoMode:
		return lipgloss.Color("129") // Purple
	}
	return lipgloss.Color("39")
}

func parseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "explore":
		return ExploreMode, true
	case "plan":
		return PlanMode, true
	case "write":
		return WriteMode, true
	case "auto":
		return AutoMode, true
	default:
		return ExploreMode, false
	}
}

var readOnlyToolNames = map[string]bool{
	"read_file":             true,
	"list_directory":        true,
	"find_files":            true,
	"grep":                  true,
	"file_info":             true,
	"get_working_directory": true,
	"read_session_notes":    true,
	"update_session_notes":  true,
	"append_session_notes":  true,
	"switch_mode":           true,
	"remember":              true,
	"recall":                true,
	"forget":                true,
	"web_fetch":             true,
	"web_search":            true,
	"web_search_api":        true,
	"web_crawl":             true,
	"get_project_tree":      true,
	"find_symbol":           true,
	"code_definition":       true,
	"code_references":       true,
	"code_hover":            true,
	"code_index":            true,
	"semantic_search":       true,
	"ask_user":              true,
	"git_status":            true,
	"git_diff":              true,
	"git_log":               true,
	"git_branch":            true,
	"git_remote":            true,
	"hash_file":             true,
	"process_list":          true,
	"disk_usage":            true,
	"spawn_subagent":        true,
}

// exploreExtraToolNames are tools available in explore mode in addition to
// readOnlyToolNames. run_shell is allowed here, but each call is filtered
// through safeshell.IsExploreReadOnlyShell before invocation.
var exploreExtraToolNames = map[string]bool{
	"run_shell": true,
}

var planExtraToolNames = map[string]bool{}

var destructiveToolNames = map[string]bool{
	"write_file":     true,
	"append_file":    true,
	"edit_file":      true,
	"delete_file":    true,
	"move_file":      true,
	"copy_file":      true,
	"make_directory": true,
	"touch":          true,
	"run_shell":      true,
	"git_add":        true,
	"git_commit":     true,
	"switch_mode":    true,
	"git_checkout":   true,
	"git_pull":       true,
	"git_push":       true,
	"git_stash":      true,
	"git_merge":      true,
	"git_reset":      true,
	"git_remote":     true,
	"git_branch":     true,
	"process_kill":   true,
	"parallel_edit":  true,
}

type modeSwitchRequest struct {
	target Mode
	mode   string
	reason string
}

func parseModeSwitchArgs(args json.RawMessage) (*modeSwitchRequest, error) {
	var a struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	target, ok := parseMode(a.Mode)
	if !ok {
		return nil, fmt.Errorf("invalid mode: %s", a.Mode)
	}
	modeName := target.String()
	reason := strings.TrimSpace(a.Reason)
	return &modeSwitchRequest{target: target, mode: modeName, reason: reason}, nil
}

func (m *Model) applyModeTransition(target Mode, reason string) bool {
	if m.mode == target {
		m.toast = fmt.Sprintf("mode: %s (%s)", m.mode, m.mode.hint())
		return false
	}

	oldMode := m.mode
	m.mode = target
	if target == PlanMode {
		m.planNotesMark = strings.TrimSpace(m.notes.get())
	}
	m.toast = fmt.Sprintf("mode: %s (%s)", m.mode, m.mode.hint())
	if strings.TrimSpace(reason) != "" {
		m.toast = fmt.Sprintf("mode: %s — %s", m.mode, strings.TrimSpace(reason))
	}

	// Routing is bound to mode, so the model — and its context window — swaps
	// here. Every real transition funnels through this function.
	if m.applyRoute(target) {
		m.toast += " · " + m.modelName
	}

	if oldMode == PlanMode && m.mode == WriteMode {
		if notes := m.notes.get(); notes != "" {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "Plan Summary from Session Notes:\n\n" + notes,
			})
		} else {
			// The model is blocked from switching without a plan; a person who
			// forces it past the gate is making their own call, but should know
			// nothing was handed off.
			m.toast = "write mode — no plan in notes, nothing was handed off"
		}
	}
	return true
}

// planGateBlocks reports whether a mode switch must be refused because the plan
// hasn't been written to notes yet. Only plan → write is gated: retreating to
// explore hands nothing off, so there is nothing to lose.
func (m *Model) planGateBlocks(target Mode) bool {
	return m.mode == PlanMode && target == WriteMode && !m.planRecorded()
}

// planGateMessage is what the model is told when the gate refuses it: an
// instruction it can act on, not just a rejection.
func (m *Model) planGateMessage() string {
	msg := `error: no plan recorded. Call update_session_notes with the complete plan — scope, the exact files to touch and the change in each, and the risks — then call switch_mode("write", ...) again.`
	if next := m.modelForMode(WriteMode); next != "" && !m.routeIsLoaded(next) {
		msg += fmt.Sprintf(" Write mode runs on %s, a different model that will see your notes but not this conversation.", next)
	}
	return msg
}

// planRecorded reports whether a plan has been written to notes since plan mode
// was entered. Emptiness alone is not enough: notes left over from an earlier
// task would pass the check while describing the wrong work.
func (m *Model) planRecorded() bool {
	notes := strings.TrimSpace(m.notes.get())
	return notes != "" && notes != m.planNotesMark
}

func (m *Model) switchModeTool() tools.Tool {
	return tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:        "switch_mode",
			Description: "Request a transition to a different mode (explore, plan, write). Use this when you have finished exploration and are ready to plan, or when your plan is approved and you need to perform write operations.",
			Parameters: tools.Schema{
				Type: "object",
				Properties: map[string]tools.Property{
					"mode": {
						Type:        "string",
						Enum:        []string{"explore", "plan", "write"},
						Description: "The target mode.",
					},
					"reason": {
						Type:        "string",
						Description: "Brief explanation of why the switch is needed.",
					},
				},
				Required: []string{"mode", "reason"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			req, err := parseModeSwitchArgs(args)
			if err != nil {
				return "", err
			}

			return fmt.Sprintf("mode switch requested to %s", req.mode), nil
		},
	}
}

var leanToolNames = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true, "append_file": true,
	"delete_file": true, "list_directory": true, "make_directory": true,
	"find_files": true, "grep": true, "file_info": true,
	"get_working_directory": true, "get_project_tree": true,
	"run_shell": true, "shell_output": true,
	"git_status": true, "git_diff": true, "git_add": true, "git_commit": true, "git_log": true,
	"switch_mode": true, "todo_write": true,
}

func (m *Model) toolsForMode() []tools.Tool {
	all := m.tools.Definitions()
	lean := m.profile.smallModel()
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		if lean && !leanToolNames[t.Function.Name] {
			continue
		}
		if m.toolAllowedInMode(t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

func (m *Model) toolAllowedInMode(name string) bool {
	return toolAllowedInMode(m.mode, name)
}

// toolAllowedInMode is the mode-gating rule as a free function so callers (e.g. a
// sub-agent goroutine) can snapshot the mode once and evaluate it off the UI
// goroutine without racing on m.mode.
func toolAllowedInMode(mode Mode, name string) bool {
	switch mode {
	case ExploreMode:
		return readOnlyToolNames[name] || exploreExtraToolNames[name]
	case PlanMode:
		return readOnlyToolNames[name] || planExtraToolNames[name]
	case WriteMode, AutoMode:
		return true
	}
	return false
}
