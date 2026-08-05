package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"sort"
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

// handOffOffloadedPlan completes a turn whose planning was offloaded to a
// tool-less provider: it records the plan the provider had no tools to write,
// switches to write mode (which routes back to the local model and injects the
// plan), and reports that the turn should continue rather than end.
//
// Narrow on purpose: plan mode only, tool-less providers only, non-empty answers
// only. A model that can call update_session_notes and switch_mode does both
// itself and is left alone.
func (m *Model) handOffOffloadedPlan(answer string) bool {
	answer = strings.TrimSpace(answer)
	if m.mode != PlanMode || m.profile.SupportsTools || answer == "" {
		return false
	}
	planner := m.modelName

	// Check BEFORE writing anything. A text that names no file is not a plan —
	// it is a question, a refusal, or progress narration. Writing it to the notes
	// anyway destroys the plan of record, and because the plan gate reads the
	// notes as evidence that a plan exists, it would then let the model into
	// write mode carrying garbage.
	check := checkPlan(answer)
	if !check.actionable() {
		m.toast = planner + " did not return an actionable plan — notes left alone"
		return false
	}
	m.notes.set(answer)

	if !m.applyModeTransition(WriteMode, "executing plan from "+planner) {
		return false
	}

	// Arm the read-before-edit gate for the files the plan claimed.
	m.planNeedsVerify = true
	m.planPaths = make(map[string]bool, len(check.named))
	for _, p := range check.named {
		m.planPaths[p] = true
	}

	// This renders in the transcript, so it doubles as the visible marker that
	// planning was offloaded and execution has come back to the local model.
	m.history = append(m.history, api.Message{
		Role: "system",
		Content: fmt.Sprintf(
			"[PLAN HANDOFF] %s planned this; %s is executing it. The planner cannot see this conversation or the outcome, so treat the plan as a proposal to verify, not an instruction to follow. %s If the code contradicts the plan, trust the code, say so, and adjust.",
			planner, m.modelName, check.findings()),
	})
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

func (m *Model) toolsForMode() []tools.Tool {
	all := m.tools.Definitions()
	lean := m.profile.smallModel()
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		if lean && !t.Policy.SmallModelSafe {
			continue
		}
		if t.Function.Name == "spawn_subagent" && !m.profile.canDelegate() {
			continue
		}
		if t.Policy.Allows(toolMode(m.mode)) {
			out = append(out, t)
		}
	}
	maxVisible := m.profile.MaxVisibleTools
	if maxVisible <= 0 && lean {
		maxVisible = 18
	}
	if maxVisible > 0 && len(out) > maxVisible {
		out = selectRelevantTools(out, m.latestUserRequest(), maxVisible)
	}
	return out
}

func (m *Model) latestUserRequest() string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "user" {
			return strings.ToLower(m.history[i].Content)
		}
	}
	return ""
}

// selectRelevantTools keeps the small-model schema budget focused while
// preserving the core inspect/edit workflow. Strong profiles normally leave
// MaxVisibleTools unset and receive every mode-allowed tool.
func selectRelevantTools(all []tools.Tool, query string, limit int) []tools.Tool {
	type ranked struct {
		tool  tools.Tool
		score int
	}
	core := map[string]int{
		"switch_mode": 100, "read_file": 99, "grep": 98, "find_files": 96,
		"list_directory": 95, "edit_file": 94, "run_shell": 93,
		"write_file": 92, "todo_write": 91, "get_project_tree": 88, "file_info": 85,
		"web_search": 82, "web_fetch": 81, "git_status": 80, "git_diff": 79,
		"shell_output": 78,
	}
	has := func(words ...string) bool {
		for _, word := range words {
			if strings.Contains(query, word) {
				return true
			}
		}
		return false
	}
	rankedTools := make([]ranked, 0, len(all))
	for _, tool := range all {
		name := tool.Function.Name
		score := core[name]
		for _, term := range strings.FieldsFunc(query, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) {
			if len(term) >= 3 && strings.Contains(strings.ToLower(name+" "+tool.Function.Description), term) {
				score += 8
			}
		}
		if has("web", "online", "latest", "current", "documentation", "url", "http") && strings.HasPrefix(name, "web_") {
			score += 50
		}
		if has("git", "commit", "branch", "merge", "diff", "repository") && strings.HasPrefix(name, "git_") {
			score += 50
		}
		if has("symbol", "definition", "reference", "function", "class", "type") &&
			(name == "find_symbol" || strings.HasPrefix(name, "code_") || name == "semantic_search") {
			score += 50
		}
		if has("process", "memory", "disk", "cpu") && (strings.HasPrefix(name, "process_") || name == "disk_usage") {
			score += 50
		}
		if has("create", "add", "implement", "fix", "change", "edit", "delete", "write") && tool.Policy.Destructive {
			score += 35
		}
		rankedTools = append(rankedTools, ranked{tool: tool, score: score})
	}
	sort.SliceStable(rankedTools, func(i, j int) bool {
		if rankedTools[i].score == rankedTools[j].score {
			return rankedTools[i].tool.Function.Name < rankedTools[j].tool.Function.Name
		}
		return rankedTools[i].score > rankedTools[j].score
	})
	out := make([]tools.Tool, 0, limit)
	for _, item := range rankedTools[:limit] {
		out = append(out, item.tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	return out
}

func (m *Model) toolAllowedInMode(name string) bool {
	return toolAllowedInMode(m.mode, name)
}

// toolAllowedInMode is the mode-gating rule as a free function so callers (e.g. a
// sub-agent goroutine) can snapshot the mode once and evaluate it off the UI
// goroutine without racing on m.mode.
func toolAllowedInMode(mode Mode, name string) bool {
	return tools.PolicyForName(name).Allows(toolMode(mode))
}

func toolMode(mode Mode) tools.ToolMode {
	switch mode {
	case ExploreMode:
		return tools.ModeExplore
	case PlanMode:
		return tools.ModePlan
	case WriteMode:
		return tools.ModeWrite
	case AutoMode:
		return tools.ModeAuto
	default:
		return 0
	}
}
