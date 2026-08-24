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
		m.planReviewRequested = ""
		m.planReviewed = ""
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

// handOffOffloadedPlan records a plan produced by a tool-less provider and
// creates the same user-review boundary ask_user would create for a native
// tool-capable planner. Execution is deliberately deferred to the next turn.
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
	m.planReviewRequested = answer
	m.history = append(m.history, api.Message{
		Role:    "assistant",
		Content: fmt.Sprintf("I recorded %s's plan. Does it match what you want? Reply `approve` to execute it, or describe one change for the planner to revise.", planner),
	})
	m.toast = "waiting for plan approval"
	return true
}

// approveOffloadedPlan recognizes an explicit approval reply and performs the
// deferred route back to the writing model. Anything else goes to the planner
// as revision feedback instead of being guessed to mean yes.
func (m *Model) approveOffloadedPlan(reply string) bool {
	if m.mode != PlanMode || m.profile.SupportsTools || m.planReviewed == "" ||
		m.planReviewed != strings.TrimSpace(m.notes.get()) || !explicitPlanApproval(reply) {
		return false
	}
	planner := m.modelName
	check := checkPlan(m.planReviewed)
	if !check.actionable() || !m.applyModeTransition(WriteMode, "approved plan from "+planner) {
		return false
	}
	m.planNeedsVerify = true
	m.planPaths = make(map[string]bool, len(check.named))
	for _, p := range check.named {
		m.planPaths[p] = true
	}
	m.history = append(m.history, api.Message{
		Role: "system",
		Content: fmt.Sprintf(
			"[PLAN HANDOFF] %s planned this; %s is executing the user-approved plan. The planner cannot see this conversation or the outcome, so verify the proposal against live code. %s If the code contradicts the plan, trust the code, say so, and adjust.",
			planner, m.modelName, check.findings()),
	})
	return true
}

func explicitPlanApproval(reply string) bool {
	reply = strings.ToLower(strings.TrimSpace(reply))
	reply = strings.Trim(reply, " .!\t\r\n")
	switch reply {
	case "approve", "approved", "yes", "y", "yes proceed", "go ahead", "looks good", "proceed":
		return true
	default:
		return false
	}
}

// planGateBlocks makes plan mode the only model-controlled route into write.
// Explore must hand off to plan first; plan must then contain a new plan that
// the user reviewed at exactly its current version. User keyboard commands can
// still force a mode directly because they do not pass through this tool gate.
func (m *Model) planGateBlocks(target Mode) bool {
	if target != WriteMode || m.mode == WriteMode {
		return false
	}
	if m.mode != PlanMode {
		return true
	}
	notes := strings.TrimSpace(m.notes.get())
	return !m.planRecorded() || m.planReviewed != notes
}

// planGateMessage is what the model is told when the gate refuses it: an
// instruction it can act on, not just a rejection.
func (m *Model) planGateMessage() string {
	if m.mode != PlanMode {
		return `error: write mode can only be requested from plan mode. Do not plan or request execution from explore. Call switch_mode("plan", ...) and let the plan-mode model produce a concrete plan for user review.`
	}
	msg := `error: no plan recorded. Call update_session_notes with the complete plan — scope, the exact files to touch and the change in each, and the risks.`
	if m.planRecorded() {
		msg = `error: the current plan has not been reviewed by the user. Summarize the concrete plan and call ask_user with one focused confirmation question. Stop and wait for their reply before requesting write mode. If their reply changes the plan, update the notes and ask for confirmation again.`
	}
	if next := m.modelForMode(WriteMode); next != "" && !m.routeIsLoaded(next) {
		msg += fmt.Sprintf(" Write mode runs on %s, a different model that will see your notes but not this conversation.", next)
	}
	return msg
}

// markPlanPresented checkpoints the plan the user is now looking at, so their
// next message counts as the review that opens the write gate. Any turn that
// ends in plan mode with a plan recorded has presented it — the model may ask
// for confirmation with ask_user or just summarize it in prose, and only the
// first path used to record the checkpoint. Without it "yes" changed nothing
// and switch_mode("write") looped against the gate until the turn ran out.
// A turn a guard stopped is the exception: it ends with "explain the blocker",
// not with a plan. Arming the checkpoint there let the user's next message — an
// unrelated follow-up question — count as the review of a plan they never saw,
// and switch_mode("write") was granted on it.
func (m *Model) markPlanPresented() {
	if m.mode == PlanMode && m.planRecorded() && !m.turnStoppedByGuard {
		m.planReviewRequested = strings.TrimSpace(m.notes.get())
	}
}

// recordPlanReview promotes the checkpoint markPlanPresented armed: the user
// has now responded to the plan they were shown, so the write gate may open.
// Both ways of responding land here — typing a message (stream.go) and picking
// an ask_user option (keys.go). Only the typed path used to promote it, so a
// user who clicked "yes, proceed" in the picker approved a plan the gate never
// heard about, and switch_mode("write") was refused until the turn died.
func (m *Model) recordPlanReview() {
	if m.planReviewRequested != "" {
		m.planReviewed = m.planReviewRequested
		m.planReviewRequested = ""
	}
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
			Description: "Request a transition to a different mode (explore, plan, write). Use this when you have finished exploration and are ready to plan, or after the user has reviewed the recorded plan and you need write mode. Never retry a denied transition or advance a changed plan without asking the user again.",
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
	if m.clarificationOnly {
		for _, tool := range all {
			if tool.Function.Name == "ask_user" {
				return []tools.Tool{tool}
			}
		}
		return nil
	}
	lean := m.profile.smallModel()
	// Workflow-critical tools survive both the small-model filter and the schema
	// cap. Without this a small model in plan mode is told it MUST record the
	// plan with update_session_notes before switch_mode is allowed — while the
	// lean filter has already removed every notes tool, so the turn cannot leave
	// plan mode at all.
	pinned := pinnedToolNames(m.mode)
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		if lean && !t.Policy.SmallModelSafe && !pinned[t.Function.Name] {
			continue
		}
		if t.Function.Name == "spawn_subagent" && !m.profile.canDelegate() {
			continue
		}
		if !t.Policy.Allows(toolMode(m.mode)) {
			continue
		}
		// Withdrawn by the repeat guard for the rest of this turn (loopguard.go).
		// A ban can never empty the toolset: switch_mode is allowed in every mode,
		// is small-model safe, and the repeat guard refuses to ban it.
		if m.bannedTools[t.Function.Name] {
			continue
		}
		out = append(out, t)
	}
	maxVisible := m.profile.MaxVisibleTools
	if maxVisible <= 0 && lean {
		maxVisible = 18
	}
	if maxVisible > 0 && len(out) > maxVisible {
		keep := make([]tools.Tool, 0, len(pinned))
		rest := make([]tools.Tool, 0, len(out))
		for _, t := range out {
			if pinned[t.Function.Name] {
				keep = append(keep, t)
			} else {
				rest = append(rest, t)
			}
		}
		out = keep
		if room := maxVisible - len(keep); room > 0 {
			out = append(out, selectRelevantTools(rest, m.latestUserRequest(), min(room, len(rest)))...)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	}
	return out
}

// pinnedToolNames returns the tools that make each mode's state machine usable.
// These names are still subject to registry, mode-policy and repeat-guard
// filtering; pinning only exempts them from schema-budget pruning.
func pinnedToolNames(mode Mode) map[string]bool {
	pinned := map[string]bool{"switch_mode": true}
	if mode == PlanMode {
		pinned["ask_user"] = true
		pinned["read_session_notes"] = true
		pinned["update_session_notes"] = true
		pinned["append_session_notes"] = true
	}
	return pinned
}

// latestUserRequest is the relevance query for selectRelevantTools, so it has
// to be the human's request: a loop-guard advisory rides the user role too, and
// letting one through would silently rewrite which tools a small model can see.
func (m *Model) latestUserRequest() string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if isUserTurn(m.history[i]) {
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
		"ask_user": 97, "list_directory": 95, "edit_file": 94, "run_shell": 93,
		"write_file": 92, "todo_write": 91, "todo_read": 90, "get_project_tree": 88, "file_info": 85,
		// The plan gate names these two; a visible-tool cap must not drop them.
		"update_session_notes": 89, "append_session_notes": 87,
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
