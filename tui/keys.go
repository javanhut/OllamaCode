package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/atotto/clipboard"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/companion"
	"github.com/javanhut/ollama_code/internal/session"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

func (m *Model) updateSettings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = stateChat
		m.input.Focus()
		return m, nil
	case "tab":
		m.cycleSettingsFocus(1)
		return m, nil
	case "shift+tab":
		m.cycleSettingsFocus(-1)
		return m, nil
	case "up":
		m.cycleSettingsFocus(-1)
		return m, nil
	case "down":
		m.cycleSettingsFocus(1)
		return m, nil
	case " ", "left", "right":
		// Switching endpoints drops whatever is typed, so it lives on its own
		// focusable row: ↑↓ walking the form can no longer wipe it by accident.
		if m.settingsFocus == settingsFocusTarget {
			targets := m.settingsTargets()
			step := 1
			if msg.String() == "left" {
				step = -1
			}
			m.settingsTarget = (m.settingsTarget + step + len(targets)) % len(targets)
			m.loadSettingsInputs()
			m.focusSettingsField(settingsFocusTarget)
			m.statusMsg = ""
			m.statusErr = false
			return m, nil
		}
		if m.settingsFocus == settingsFocusTrust {
			m.settingsTrust = !m.settingsTrust
			return m, nil
		}
		if m.settingsFocus == settingsFocusNative {
			step := 1
			if msg.String() == "left" {
				step = -1
			}
			i := max(slices.Index(providerKinds, m.settingsKind), 0)
			m.settingsKind = providerKinds[(i+step+len(providerKinds))%len(providerKinds)]
			// The Trust row only exists for the cursor kind; cycling away from it
			// would otherwise strand focus on a row that is no longer rendered.
			if !slices.Contains(m.settingsFields(), m.settingsFocus) {
				m.focusSettingsField(settingsFocusNative)
			}
			return m, nil
		}
	case "ctrl+d":
		return m, m.deleteSettingsProvider()
	case "enter":
		host, err := m.saveSettingsInputs()
		if err != nil {
			m.statusMsg = err.Error()
			m.statusErr = true
			return m, nil
		}
		m.statusMsg = "connecting…"
		m.statusErr = false
		// Probe the endpoint just edited, not the active one — the point of
		// typing a key is finding out whether it works.
		return m, m.fetchModelsFrom(host, m.settingsTargetName())
	}

	var cmd tea.Cmd
	switch m.settingsFocus {
	case settingsFocusName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case settingsFocusKey:
		m.keyInput, cmd = m.keyInput.Update(msg)
	case settingsFocusEnv:
		m.envInput, cmd = m.envInput.Update(msg)
	case settingsFocusTarget, settingsFocusNative, settingsFocusTrust: // selector rows: nothing to type into
	default:
		m.urlInput, cmd = m.urlInput.Update(msg)
	}
	return m, cmd
}

// deleteSettingsProvider removes the provider under the cursor along with any
// routes bound to it. The default host and the blank new-provider slot are not
// deletable, so ctrl+d is a no-op there.
func (m *Model) deleteSettingsProvider() tea.Cmd {
	name := m.settingsTargetName()
	if name == "" {
		return nil
	}
	delete(m.cfg.Providers, name)
	m.clearRoutesFor(name)
	saveConfig(m.cfg)
	m.reloadActiveHost()
	m.settingsTarget = 0
	m.loadSettingsInputs()
	m.statusMsg = "removed " + name
	m.statusErr = false
	return nil
}

// focusSettingsField moves focus to one row, blurring the rest.
func (m *Model) focusSettingsField(f settingsField) {
	m.settingsFocus = f
	m.nameInput.Blur()
	m.urlInput.Blur()
	m.keyInput.Blur()
	m.envInput.Blur()
	switch f {
	case settingsFocusName:
		m.nameInput.Focus()
	case settingsFocusURL:
		m.urlInput.Focus()
	case settingsFocusKey:
		m.keyInput.Focus()
	case settingsFocusEnv:
		m.envInput.Focus()
	}
}

// cycleSettingsFocus walks the rows the selected endpoint actually has.
func (m *Model) cycleSettingsFocus(step int) {
	fields := m.settingsFields()
	i := max(slices.Index(fields, m.settingsFocus), 0)
	m.focusSettingsField(fields[(i+step+len(fields))%len(fields)])
}

// updateJobs drives the /jobs modal: a flat list of background shell jobs and
// sub-agent jobs with a per-row kill. Killing needs no confirmation — the jobs
// are user-started and SIGKILL/cancel is the existing semantic everywhere else
// (shell_output(kill=true), esc during a sub-agent run).
func (m *Model) updateJobs(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	rows := m.jobRows()
	switch msg.String() {
	case "up", "k":
		if m.jobsCursor > 0 {
			m.jobsCursor--
		}
	case "down", "j":
		if m.jobsCursor < len(rows)-1 {
			m.jobsCursor++
		}
	case "x", "d":
		if len(rows) == 0 || m.jobsCursor >= len(rows) {
			return m, nil
		}
		row := rows[m.jobsCursor]
		switch {
		case row.done:
			m.toast = fmt.Sprintf("job %d already finished", row.id)
		case row.shell:
			if _, err := jobRegistry().Cancel(row.id); err != nil {
				m.toast = err.Error()
			} else {
				m.toast = fmt.Sprintf("job %d killed", row.id)
			}
		case m.subagents != nil && m.subagents.cancel(row.id):
			m.toast = fmt.Sprintf("sub-agent job %d cancelled", row.id)
		default:
			m.toast = fmt.Sprintf("no running sub-agent job %d", row.id)
		}
	case "r":
		// The rows are read live on every paint; this is just an explicit nudge.
		m.toast = "refreshed"
	case "esc", "q", "enter":
		m.state = stateChat
		m.input.Focus()
	}
	return m, nil
}

func (m *Model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// A pull is streaming: only allow cancel.
	if m.pulling {
		if msg.String() == "esc" {
			m.cancelPull()
			m.pullErr = "pull canceled"
		}
		return m, nil
	}

	// Name-entry mode: typing the model to pull.
	if m.pullInput.Focused() {
		switch msg.String() {
		case "esc":
			m.pullInput.Blur()
			m.pullInput.Reset()
			m.pullErr = ""
			return m, nil
		case "enter":
			name := strings.TrimSpace(m.pullInput.Value())
			if name == "" {
				return m, nil
			}
			m.pullInput.Blur()
			m.pullInput.Reset()
			return m, m.startPull(name)
		}
		var cmd tea.Cmd
		m.pullInput, cmd = m.pullInput.Update(msg)
		return m, cmd
	}

	// Browse mode.
	switch msg.String() {
	case "up", "k":
		if m.picker > 0 {
			m.picker--
		}
		return m, nil
	case "down", "j":
		if m.picker < len(m.models)-1 {
			m.picker++
		}
		return m, nil
	case "left", "right":
		// Browse another endpoint's models. The pairing flow is mid-way through a
		// two-step choice and owns which endpoint it lists.
		targets := m.pickerTargets()
		if m.pickerPurpose != "" || len(targets) < 2 {
			return m, nil
		}
		step := 1
		if msg.String() == "left" {
			step = -1
		}
		m.pickerTarget = (m.pickerTarget + step + len(targets)) % len(targets)
		m.models = nil
		m.picker = 0
		m.statusMsg = "loading…"
		m.statusErr = false
		return m, m.fetchModels()
	case "esc":
		m.state = stateChat
		m.input.Focus()
		return m, nil
	case "r":
		m.statusMsg = "refreshing…"
		m.statusErr = false
		if m.pickerPurpose == "cursor_pair" && m.pairCursor != "" {
			return m, m.fetchModelsFrom(m.providerHost(m.pairCursor), m.pairCursor)
		}
		return m, m.fetchModels()
	case "p":
		if m.pickerPurpose == "cursor_pair" || m.modelsFrom != "" {
			return m, nil
		}
		m.pullErr = ""
		return m, m.pullInput.Focus()
	case "c":
		if m.pickerPurpose == "cursor_pair" || m.modelsFrom != "" || len(m.models) == 0 {
			return m, nil
		}
		provider := m.cursorPlanProvider()
		if provider == "" {
			m.toast = "no Cursor provider configured — add one with /provider new"
			return m, nil
		}
		m.pairLocalModel = m.models[m.picker]
		m.pairCursor = provider
		m.pickerPurpose = "cursor_pair"
		m.statusMsg = "loading Cursor planning models…"
		m.statusErr = false
		return m, m.fetchModelsFrom(m.providerHost(provider), provider)
	case "enter":
		if len(m.models) == 0 {
			return m, nil
		}
		if m.pickerPurpose == "cursor_pair" {
			m.configureCursorPair(m.pairLocalModel, m.pairCursor, m.models[m.picker])
		} else {
			m.selectModel(m.models[m.picker], m.modelsFrom)
		}
		m.pickerPurpose, m.pairLocalModel, m.pairCursor = "", "", ""
		m.state = stateChat
		m.input.Focus()
		m.layout()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m, nil
}

// rateCommand implements /rate: a human verdict on the turn that just
// finished. The rating rides the existing redacted trace as a turn_rating
// event, so cmd/finetune picks it up in the pass it already makes over the
// file. from_turn is what actually names the turn: startStream bumps turnGen
// once per tool round, so a user turn is a RANGE of generations whose last one
// holds only the final prose reply — rating that generation alone would rate
// the one group the exporter already throws away.
func (m *Model) rateCommand(args string) {
	verdict, note, _ := strings.Cut(args, " ")
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	note = strings.TrimSpace(note)
	// Above the empty-verdict branch: bare /rate must not answer "unrated",
	// which reads as an invitation, in the states where the verdict it invites
	// would then be refused.
	if m.ratedTo == 0 {
		m.toast = "no completed turn to rate yet"
		return
	}
	if verdict == "" {
		if m.turnRating == "" {
			m.toast = "last turn is unrated — /rate good|bad [note]"
		} else {
			m.toast = "last turn rated " + m.turnRating
		}
		return
	}
	if verdict != "good" && verdict != "bad" {
		m.toast = "usage: /rate good|bad [note]"
		return
	}
	// Swallowing a rating silently would be worse than refusing it: the whole
	// point of the keystroke is that the signal reaches the dataset.
	if m.trace == nil {
		m.toast = `rating not recorded — tracing is off (set "trace": true in config)`
		return
	}
	meta := map[string]any{"turn": m.ratedTo, "from_turn": m.ratedFrom, "rating": verdict}
	if note != "" {
		meta["note"] = note
	}
	// Event.Turn is deliberately left zero: the exporter opens a trajectory
	// group for any event carrying a turn, and a rating must not become one.
	_ = m.trace.Record(tracepkg.Event{Kind: "turn_rating", Model: m.modelName, Metadata: meta})
	m.turnRating = verdict
	m.toast = "turn rated " + verdict
}

// modelUsage is shown whenever /model args don't parse.
const modelUsage = "usage: /model [use <name>] [ctx <tokens>] [temp <0.0-2.0>] — no args shows current model settings"

// modelInfoCommand implements /model: the per-model settings surface. No args
// prints the active model's configuration; args change it. /models stays the
// interactive list/switch/pull picker — the two commands deliberately do
// different things.
func (m *Model) modelInfoCommand(args string) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		m.showModelInfo()
		return
	}
	name := ""
	switch fields[0] {
	case "use":
		if len(fields) != 2 {
			m.toast = modelUsage
			return
		}
		name = fields[1]
	case "ctx":
		if len(fields) != 2 {
			m.toast = modelUsage
			return
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1024 {
			m.toast = "ctx must be a number ≥ 1024"
			return
		}
		p := m.profile
		p.NumCtx = n
		// An explicit override outranks a ceiling learned from an earlier
		// allocation failure — the GPU may have freed up since.
		m.host.ForgetContextCeiling(m.modelName)
		m.saveProfile(p)
		m.toast = fmt.Sprintf("num_ctx for %s set to %d", m.modelName, m.contextLimit)
		return
	case "temp":
		if len(fields) != 2 {
			m.toast = modelUsage
			return
		}
		f, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || f < 0 || f > 2 {
			m.toast = "temp must be a number between 0.0 and 2.0"
			return
		}
		p := m.profile
		p.Temperature = &f
		m.saveProfile(p)
		m.toast = fmt.Sprintf("temperature for %s set to %.2f", m.modelName, f)
		return
	default:
		// A bare name is shorthand for "/model use <name>".
		if len(fields) == 1 {
			name = fields[0]
		} else {
			m.toast = modelUsage
			return
		}
	}
	// A bare name goes to the default host; "provider:model" is honored too, so
	// /model use can target a provider without going through the picker.
	provider, bare := m.splitRouteSpec(name)
	m.selectModel(bare, provider)
	m.toast = "default model set to " + name
	// A route for the current mode outranks this on the next mode switch; say so
	// now rather than letting the model appear to change back on its own.
	if bound := strings.TrimSpace(m.cfg.Routes[m.mode.String()]); bound != "" && bound != name {
		m.toast += fmt.Sprintf(" — %s mode is routed to %s and will switch back", m.mode, bound)
	}
}

// saveProfile persists a profile override for the current model and applies it.
func (m *Model) saveProfile(p ModelProfile) {
	if m.cfg.Profiles == nil {
		m.cfg.Profiles = map[string]ModelProfile{}
	}
	m.cfg.Profiles[m.modelName] = p
	saveConfig(m.cfg)
	m.applyProfile(p)
}

// showModelInfo renders the active model's settings into the transcript.
func (m *Model) showModelInfo() {
	if m.modelName == "" {
		m.toast = "no model selected — use /models to pick one"
		return
	}
	p := m.profile
	var b strings.Builder
	fmt.Fprintf(&b, "Current model: %s\n", m.modelName)
	fmt.Fprintf(&b, "- context (num_ctx): %d tokens\n", m.contextLimit)
	if p.ParamsB > 0 {
		fmt.Fprintf(&b, "- parameters: %.1fB\n", p.ParamsB)
	}
	tier := p.CapabilityTier
	if tier == "" {
		if p.smallModel() {
			tier = "small (inferred)"
		} else {
			tier = "capable (inferred)"
		}
	}
	fmt.Fprintf(&b, "- capability tier: %s · parallel tools: %t (max %d)\n", tier, p.parallelToolCalls(), p.maxParallelToolCalls())
	fmt.Fprintf(&b, "- step budget: %d · delegation: %t · review pass: %t\n", m.turnStepLimit(), p.canDelegate(), p.reviewPass())
	fmt.Fprintf(&b, "- tools: %t · thinking: %t\n", p.SupportsTools, p.SupportsThinking)
	temp := "ollama default"
	if p.Temperature != nil {
		temp = fmt.Sprintf("%.2f", *p.Temperature)
	} else if p.smallModel() {
		temp = "0.00 action / 0.20 prose (auto: small model)"
	}
	fmt.Fprintf(&b, "- temperature: %s\n", temp)
	topK, ragTokens := m.ragLimits()
	fmt.Fprintf(&b, "- RAG: top %d · %d tokens\n", topK, ragTokens)
	if p.TopP != nil {
		fmt.Fprintf(&b, "- top_p: %.2f\n", *p.TopP)
	}
	if p.NumPredict != nil {
		fmt.Fprintf(&b, "- num_predict: %d\n", *p.NumPredict)
	}
	if len(m.cfg.Routes) > 0 {
		fmt.Fprintf(&b, "- routed: this is the model bound to %s mode (/route to see the table)\n", m.mode)
	}
	b.WriteString("Change with: /model use <name> · /model ctx <tokens> · /model temp <value> — or /models to list, switch, and pull.")
	m.history = append(m.history, api.Message{Role: "system", Content: b.String()})
	m.refreshTranscript()
	m.viewport.GotoBottom()
}

// cancelPull aborts an in-flight model download and clears the streaming state.

// updateQuestion drives the ask_user option picker. Esc does not cancel the
// question — there is nothing to cancel, the model is already waiting — it just
// closes the picker so the answer can be typed instead, which is what an option
// list that does not cover the real answer needs. A typed answer goes through
// submit() as a user message and the parked tool result keeps its placeholder,
// so each question is answered by exactly one delivery path.
func (m *Model) updateQuestion(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	options := m.question.Options
	if len(options) == 0 {
		m.state = stateChat
		m.input.Focus()
		return m, nil
	}
	switch key := msg.String(); key {
	case "up", "k":
		if m.questionCursor > 0 {
			m.questionCursor--
		}
		return m, nil
	case "down", "j":
		if m.questionCursor < len(options)-1 {
			m.questionCursor++
		}
		return m, nil
	case " ", "space":
		// Space toggles the focused row's checkbox; single-select questions
		// ignore it, so space never eats a keypress that means nothing.
		if !m.question.MultiSelect {
			return m, nil
		}
		if m.questionChecked == nil {
			m.questionChecked = map[int]bool{}
		}
		m.questionChecked[m.questionCursor] = !m.questionChecked[m.questionCursor]
		return m, nil
	case "esc":
		m.state = stateChat
		m.input.Focus()
		m.toast = "type your answer"
		return m, nil
	case "enter":
		if m.question.MultiSelect {
			return m.resolveQuestion(m.multiSelectAnswer())
		}
		if m.questionCursor < 0 || m.questionCursor >= len(options) {
			return m, nil
		}
		return m.resolveQuestion([]string{options[m.questionCursor]})
	default:
		choice, ok := chooseQuestionOption(key, m.questionCursor, options)
		if !ok {
			return m, nil
		}
		return m.resolveQuestion([]string{choice})
	}
}

// checkedQuestionOptions returns the toggled labels in list order, so the
// answer reads the same regardless of the order the user toggled them in.
func (m *Model) checkedQuestionOptions() []string {
	var labels []string
	for i, opt := range m.question.Options {
		if m.questionChecked[i] {
			labels = append(labels, opt)
		}
	}
	return labels
}

// multiSelectAnswer is the label set enter confirms: the toggled rows, or the
// focused row when nothing is toggled — the sensible default, exactly what
// enter means in the single-select picker.
func (m *Model) multiSelectAnswer() []string {
	labels := m.checkedQuestionOptions()
	if len(labels) == 0 && m.questionCursor >= 0 && m.questionCursor < len(m.question.Options) {
		labels = []string{m.question.Options[m.questionCursor]}
	}
	return labels
}

// resolveQuestion delivers a picked answer as the ask_user tool result and
// resumes the turn. The batch parked when the picker opened (dispatch.go), so
// starting a stream here is exactly what the batch loop would have done had
// ask_user returned this answer on its own.
func (m *Model) resolveQuestion(labels []string) (tea.Model, tea.Cmd) {
	m.applyQuestionAnswer(strings.Join(labels, ", "))
	cmd := m.startStream()
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return m, cmd
}

// applyQuestionAnswer rewrites the parked ask_user result with the answer and
// closes the picker. No user message is staged: the answer belongs to the tool
// call that asked for it, and a synthetic user message would both duplicate it
// and reset the turn guards the resumed turn still lives under.
func (m *Model) applyQuestionAnswer(answer string) {
	if m.questionResult >= 0 && m.questionResult < len(m.history) &&
		m.history[m.questionResult].Role == "tool" && m.history[m.questionResult].ToolName == "ask_user" {
		m.history[m.questionResult].Content = "ANSWER: " + answer
	}
	m.recordPlanReview()
	m.state = stateChat
	m.question = tools.AskUserQuestion{}
	m.questionChecked = nil
	m.questionResult = -1
	m.input.Focus()
}

// orderedQuestionOptions returns the options with the recommended one first.
// The label is only moved, never duplicated or rewritten, so the answer that
// comes back is still exactly one of the model's own labels.
func orderedQuestionOptions(q tools.AskUserQuestion) []string {
	if q.Recommended == "" {
		return q.Options
	}
	for i, opt := range q.Options {
		if opt == q.Recommended {
			out := make([]string, 0, len(q.Options))
			out = append(out, opt)
			out = append(out, q.Options[:i]...)
			out = append(out, q.Options[i+1:]...)
			return out
		}
	}
	return q.Options
}

// chooseQuestionOption maps a keypress to the option it selects. ok=false means
// the key selects nothing — it is navigation, or a digit naming an option this
// question does not have, which must do nothing rather than submit an
// out-of-range choice.
//
// Separated from the send so the mapping is testable without a live model host.
func chooseQuestionOption(key string, cursor int, options []string) (string, bool) {
	if len(options) == 0 {
		return "", false
	}
	if key == "enter" {
		if cursor < 0 || cursor >= len(options) {
			return "", false
		}
		return options[cursor], true
	}
	// Single digits pick directly; 1-9 only, because a two-key number would
	// need a commit keystroke and this list is never that long.
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if i := int(key[0] - '1'); i < len(options) {
			return options[i], true
		}
	}
	return "", false
}

// updateLoopGuard drives the doom-loop escalation modal. Esc picks the safe
// default — "stop turn", exactly what the guards used to do on their own —
// because every other choice keeps a looping agent running.
func (m *Model) updateLoopGuard(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	esc := m.loopEscalation
	if esc == nil {
		m.state = stateChat
		return m, nil
	}
	options := loopEscalationOptions(esc)
	switch key := msg.String(); key {
	case "up", "k":
		if m.loopGuardCursor > 0 {
			m.loopGuardCursor--
		}
		return m, nil
	case "down", "j":
		if m.loopGuardCursor < len(options)-1 {
			m.loopGuardCursor++
		}
		return m, nil
	case "esc":
		return m.resolveLoopEscalation(0)
	default:
		choice, ok := chooseLoopGuardOption(key, m.loopGuardCursor, options)
		if !ok {
			return m, nil
		}
		return m.resolveLoopEscalation(choice)
	}
}

// loopEscalationOptions lists the user's choices. "Ban tool & continue" only
// exists when the loop indicts one bannable tool — a pure oscillation or
// stagnation has no single culprit to take away.
func loopEscalationOptions(esc *loopEscalation) []string {
	options := []string{"Stop turn", "Continue anyway"}
	if loopBanAllowed(esc.tool) {
		options = append(options, fmt.Sprintf("Ban %s & continue", esc.tool))
	}
	return options
}

// chooseLoopGuardOption maps a keypress to a choice index. ok=false means the
// key selects nothing — navigation, or a digit naming a choice this modal
// does not have, which must do nothing rather than act out of range.
func chooseLoopGuardOption(key string, cursor int, options []string) (int, bool) {
	if len(options) == 0 {
		return 0, false
	}
	if key == "enter" {
		if cursor < 0 || cursor >= len(options) {
			return 0, false
		}
		return cursor, true
	}
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if i := int(key[0] - '1'); i < len(options) {
			return i, true
		}
	}
	return 0, false
}

// resolveLoopEscalation applies the user's choice and resumes the turn. All
// three paths end in startStream: the stop path streams the blocker report,
// the other two stream the agent's next working round.
func (m *Model) resolveLoopEscalation(choice int) (tea.Model, tea.Cmd) {
	esc := m.loopEscalation
	m.loopEscalation = nil
	m.state = stateChat
	m.toast = ""
	switch choice {
	case 0: // Stop turn — exactly the automatic path.
		m.applyLoopGuardStops(esc)
	case 2: // Ban the tool for the rest of the turn, then keep working.
		m.bannedTools[esc.tool] = true
		m.resetLoopDetector(esc.kind)
		m.history = append(m.history, advisory(fmt.Sprintf("[TOOL DISABLED] You called %q %d times in a row without making progress, so it is removed from your tools for the rest of this turn — calling it in text will be refused too. Finish with the tools you still have, or answer the user in plain text.", esc.tool, esc.streak)))
	default: // Continue anyway — reset only the detector that fired.
		if m.loopContinues == nil {
			m.loopContinues = map[string]int{}
		}
		m.loopContinues[esc.kind]++
		m.resetLoopDetector(esc.kind)
	}
	cmd := m.startStream()
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return m, cmd
}

func (m *Model) updatePermission(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		m.state = stateChat
		return m, nil
	}
	switch msg.String() {
	case "y", "enter":
		i := m.pending.index
		call := m.pending.calls[i]
		m.recordPermission(call, "allowed_once")
		m.pending.started[i] = true
		m.state = stateChat
		return m, m.invokeToolCmd(m.pending.gen, i, call)
	case "a":
		for i, call := range m.pending.calls {
			if !m.pending.started[i] {
				m.recordPermission(call, "allowed_for_batch")
			}
		}
		m.pending.allowAll = true
		m.state = stateChat
		return m, m.processPendingTools()
	case "A":
		// Persist the decision, then behave exactly like "y". This widens the
		// safety boundary beyond the current turn, so the rule it saves is shown
		// in the modal before the key is pressed.
		i := m.pending.index
		call := m.pending.calls[i]
		rule := tools.PermissionRuleFor(call)
		m.savePermissionRule(rule)
		m.recordPermission(call, "allowed_by_new_rule")
		m.pending.started[i] = true
		m.state = stateChat
		return m, m.invokeToolCmd(m.pending.gen, i, call)
	case "n", "esc":
		i := m.pending.index
		call := m.pending.calls[i]
		m.recordPermission(call, "denied")
		// A denial ends this tool round. Count it as a failure for diagnostics,
		// cancel every call that has not started, and let any already-running calls
		// drain before processPendingTools finalizes the turn and asks for feedback.
		fp := tools.CallFingerprint(call)
		m.failedCalls[fp]++
		m.pending.deniedTool = call.Function.Name
		m.pending.results[i] = api.Message{
			Role:     "tool",
			ToolName: call.Function.Name,
			Content:  "denied by user. The turn has stopped. Do NOT retry this call or a minor variant unless the user explicitly requests it later.",
		}
		m.pending.started[i] = true
		m.pending.done++
		for j, pendingCall := range m.pending.calls {
			if m.pending.started[j] {
				continue
			}
			m.pending.results[j] = api.Message{
				Role:     "tool",
				ToolName: pendingCall.Function.Name,
				Content:  "not run because the user denied another call in this batch and ended the turn.",
			}
			m.pending.started[j] = true
			m.pending.done++
		}
		m.state = stateChat
		cmd := m.processPendingTools()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return m, cmd
	}
	return m, nil
}

func (m *Model) updateChatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		if val == "" {
			return m, nil
		}

		if m.streaming && !strings.HasPrefix(val, "/") {
			m.queue = append(m.queue, val)
			m.input.Reset()
			m.slashVisible = false
			m.slashSuggestions = nil
			m.dismissMention()
			m.toast = fmt.Sprintf("queued (%d in queue)", len(m.queue))
			return m, nil
		}

		m.slashVisible = false
		m.slashSuggestions = nil
		m.dismissMention()
		m.toast = ""
		if val == "/clearnotes" || val == "/notes clear" || val == "/notes reset" {
			m.input.Reset()
			m.notes.set("")
			m.notesViewport.SetContent(m.renderNotesMarkdown("(empty)", m.notesViewport.Width()))
			m.toast = "session notes cleared"
			m.refreshTranscript()
			return m, nil
		}
		if val == "/notes restore" {
			m.input.Reset()
			if m.notesBackup == "" {
				m.toast = "no notes backup to restore"
				return m, nil
			}
			m.notes.set(m.notesBackup)
			m.notesViewport.SetContent(m.renderNotesMarkdown(m.notesBackup, m.notesViewport.Width()))
			m.notesBackup = ""
			m.toast = "notes restored from pre-dream backup"
			m.refreshTranscript()
			return m, nil
		}
		if val == "/mode" || strings.HasPrefix(val, "/mode ") {
			m.input.Reset()
			args := strings.TrimSpace(strings.TrimPrefix(val, "/mode"))
			if target, ok := parseMode(args); ok {
				m.applyModeTransition(target, "switched by user")
				m.refreshTranscript()
				m.viewport.GotoBottom()
			} else {
				m.toast = "invalid mode: " + args + " (choose explore, plan, write, auto)"
			}
			return m, nil
		}
		if val == "/model" || strings.HasPrefix(val, "/model ") {
			m.input.Reset()
			args := strings.TrimSpace(strings.TrimPrefix(val, "/model"))
			if args == "calibrate" {
				if m.modelName == "" {
					m.toast = "select a model first"
					return m, nil
				}
				m.toast = "calibrating tool behavior…"
				return m, m.calibrateModelCmd()
			}
			if args == "calibrate apply" {
				m.applyCalibration()
				return m, nil
			}
			m.modelInfoCommand(args)
			return m, nil
		}
		if val == "/route" || strings.HasPrefix(val, "/route ") {
			m.input.Reset()
			m.routeCommand(strings.TrimSpace(strings.TrimPrefix(val, "/route")))
			return m, nil
		}
		if val == "/settings" || strings.HasPrefix(val, "/settings ") {
			m.input.Reset()
			m.openSettings(strings.TrimSpace(strings.TrimPrefix(val, "/settings")))
			return m, nil
		}
		if val == "/provider" || strings.HasPrefix(val, "/provider ") {
			m.input.Reset()
			m.providerCommand(strings.TrimSpace(strings.TrimPrefix(val, "/provider")))
			return m, nil
		}
		if val == "/save" || strings.HasPrefix(val, "/save ") {
			m.input.Reset()
			m.saveCommand(strings.TrimSpace(strings.TrimPrefix(val, "/save")))
			return m, nil
		}
		if val == "/load" || strings.HasPrefix(val, "/load ") {
			m.input.Reset()
			m.loadCommand(strings.TrimSpace(strings.TrimPrefix(val, "/load")))
			return m, nil
		}
		if val == "/fork" || strings.HasPrefix(val, "/fork ") {
			m.input.Reset()
			m.forkCommand(strings.TrimSpace(strings.TrimPrefix(val, "/fork")))
			return m, nil
		}
		if val == "/rename" || strings.HasPrefix(val, "/rename ") {
			m.input.Reset()
			m.renameCommand(strings.TrimSpace(strings.TrimPrefix(val, "/rename")))
			return m, nil
		}
		if val == "/title" || strings.HasPrefix(val, "/title ") {
			m.input.Reset()
			m.titleCommand(strings.TrimSpace(strings.TrimPrefix(val, "/title")))
			return m, nil
		}
		if val == "/rewind" || strings.HasPrefix(val, "/rewind ") {
			m.input.Reset()
			m.rewindCommand(strings.TrimSpace(strings.TrimPrefix(val, "/rewind")))
			return m, nil
		}
		if val == "/rate" || strings.HasPrefix(val, "/rate ") {
			m.input.Reset()
			m.rateCommand(strings.TrimSpace(strings.TrimPrefix(val, "/rate")))
			return m, nil
		}
		if val == "/research" || strings.HasPrefix(val, "/research ") {
			m.input.Reset()
			return m, m.researchCommand(strings.TrimSpace(strings.TrimPrefix(val, "/research")))
		}
		switch val {
		case "/auto":
			m.input.Reset()
			m.applyModeTransition(AutoMode, "switched by user")
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return m, nil
		case "/quit", "/exit":
			return m, tea.Quit

		case "/stash":
			// No input.Reset() here: the draft being typed is the payload.
			m.stashCommand()
			return m, nil
		case "/unstash":
			m.input.Reset()
			m.unstashCommand()
			return m, nil

		case "/models":
			m.input.Reset()
			m.pickerPurpose, m.pairLocalModel, m.pairCursor = "", "", ""
			m.pickerTarget = max(slices.Index(m.pickerTargets(), m.activeProvider()), 0)
			m.statusMsg = "refreshing…"
			m.statusErr = false
			return m, m.fetchModels()
		case "/clear":
			m.input.Reset()
			if m.streaming && m.stream != nil {
				m.stream.cancel()
			}
			m.turnGen++ // orphan any in-flight stream/tool messages
			m.streamBuf.Reset()
			m.streaming = false
			m.stream = nil
			m.busySince = time.Time{}
			m.pending = nil
			m.queue = nil
			m.history = nil
			m.archiveSummary, m.archivedThrough, m.prunedThrough = "", 0, 0
			m.contextSnapshot = nil // no history left to hold the baseline
			m.sessionName, m.sessionTitle = "", ""
			m.titlePinned, m.titleGenTried = false, false
			m.turnRecords = nil
			m.ratedFrom, m.ratedTo, m.turnRating = 0, 0, "" // the rateable turn is part of the conversation being cleared
			m.historyIndex = len(m.userHistory)
			m.lastError = ""
			m.mentionBlock = "" // attachments belong to the conversation just cleared
			m.routeDeclines = 0 // new conversation: the routing offer is worth making again
			m.refreshTranscript()
			m.viewport.GotoTop()
			return m, nil
		case "/dream":
			m.input.Reset()
			on := !m.dreamsOn()
			m.cfg.Dream = &on
			saveConfig(m.cfg)
			if !on {
				m.wake()
				m.toast = "dream mode off"
			} else {
				m.toast = "dream mode on — I'll reflect after 3 min idle"
			}
			return m, nil
		case "/face":
			m.input.Reset()
			on := !m.faceOn()
			m.cfg.Face = &on
			saveConfig(m.cfg)
			if on {
				m.toast = "face on"
			} else {
				m.toast = "face off"
			}
			return m, nil
		case "/welcome":
			m.input.Reset()
			on := !m.welcomeOn()
			m.cfg.Welcome = &on
			saveConfig(m.cfg)
			if on {
				m.toast = "welcome panel on"
			} else {
				m.toast = "welcome panel off"
			}
			m.refreshTranscript()
			return m, nil
		case "/dreams":
			m.input.Reset()
			m.history = append(m.history, api.Message{Role: "system", Content: m.dreamLog()})
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return m, nil
		case "/verify":
			m.input.Reset()
			on := !m.verifyOn()
			m.cfg.Verify = &on
			saveConfig(m.cfg)
			if on {
				cmd, label, ok := m.verifyCommand()
				if ok {
					m.toast = "verify on — will run `" + cmd + "` (" + label + ") after edits"
				} else {
					m.toast = "verify on — no auto-check for this project; set verify_cmd in config"
				}
			} else {
				m.toast = "verify off"
			}
			return m, nil
		case "/undo":
			m.input.Reset()
			summary, touched := m.undoLast()
			m.toast = summary
			for _, p := range touched {
				m.noteFileChanged([]string{p}) // keep the RAG index in sync
			}
			m.noteUndoToModel(touched)
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return m, nil
		case "/help", "/?":
			m.input.Reset()
			m.helpViewport.SetContent(m.helpContent(m.helpViewport.Width()))
			m.helpViewport.GotoTop()
			m.state = stateHelp
			return m, nil
		case "/notes":
			m.input.Reset()
			m.showNotes = !m.showNotes
			m.layout()
			return m, nil
		case "/diff":
			m.input.Reset()
			d := m.lastTurnDiffs()
			if d == "" {
				m.toast = "no file diffs in the last turn"
				return m, nil
			}
			m.diffSource = d
			m.diffViewport.SetContent(colorizeDiff(d, m.diffViewport.Width()))
			m.diffViewport.GotoTop()
			m.state = stateDiff
			return m, nil
		case "/companion":
			m.input.Reset()
			if m.companion != nil {
				_ = m.companion.Close()
				m.companion = nil
				m.toast = "companion stopped"
				return m, nil
			}
			client, err := companion.Start()
			if err != nil {
				m.toast = "companion: " + err.Error()
				return m, nil
			}
			m.companion = client
			m.toast = "companion started — speak to type"
			send := m.companionSender
			go func() {
				// p.Send can panic if the program has already shut down; never let
				// that crash the process.
				defer func() { _ = recover() }()
				if send == nil {
					return
				}
				for {
					select {
					case t, ok := <-client.Transcripts:
						if !ok {
							send(companionStoppedMsg{})
							return
						}
						send(companionTranscriptMsg{text: t.Text})
					case e, ok := <-client.Errors:
						if !ok {
							return
						}
						send(companionErrorMsg{err: e})
					}
				}
			}()
			return m, nil
		case "/copy":
			m.input.Reset()
			text := lastAssistantMessage(m.history)
			if text == "" {
				m.toast = "nothing to copy"
				return m, nil
			}
			if err := clipboard.WriteAll(text); err != nil {
				m.toast = fmt.Sprintf("clipboard error: %v", err)
				return m, nil
			}
			m.toast = fmt.Sprintf("copied %d chars to clipboard", len(text))
			return m, nil
		case "/verbose":
			m.input.Reset()
			m.cfg.Verbose = !m.cfg.Verbose
			saveConfig(m.cfg)
			if m.cfg.Verbose {
				m.toast = "verbose mode on"
			} else {
				m.toast = "verbose mode off"
			}
			m.refreshTranscript()
			return m, nil
		case "/stats":
			m.input.Reset()
			m.state = stateStats
			return m, nil
		case "/compact":
			m.input.Reset()
			// force: the user asked for it, so a pruning pass that happens to clear
			// the automatic threshold must not cancel the summary they wanted.
			if cmd := m.compactContext(true); cmd != nil {
				return m, cmd // the toast is set inside compactContext
			}
			if m.compacting {
				m.toast = "already compacting"
			} else {
				m.toast = "history too short to compact"
			}
			return m, nil
		case "/jobs":
			m.input.Reset()
			m.jobsCursor = 0
			m.state = stateJobs
			return m, nil
		case "/show_thinking", "/thinking":
			m.input.Reset()
			m.cfg.Thinking = !m.cfg.Thinking
			saveConfig(m.cfg)
			if m.cfg.Thinking {
				m.toast = "thinking shown — live and completed reasoning is isolated above answers"
			} else {
				m.toast = "thinking hidden"
			}
			m.refreshTranscript()
			return m, nil
		case "/archive":
			m.input.Reset()
			if m.kvStore == nil {
				m.toast = "archive not initialized"
				return m, nil
			}
			// Just show the most recent archive for demo. LatestKey parses the
			// archive_<unix> suffix numerically — lexical order breaks across
			// digit-width boundaries.
			lastKey, ok := m.kvStore.LatestKey("archive_")
			if !ok {
				m.toast = "no archives found"
				return m, nil
			}
			val, _ := m.kvStore.Get(lastKey)
			archived, _ := val.(string)
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "ARCHIVE (" + lastKey + "):\n\n" + archived,
			})
			m.refreshTranscript()
			m.viewport.GotoBottom()
			m.toast = "retrieved archive"
			return m, nil
		case "/sessions":
			m.input.Reset()
			sessions, err := session.List()
			if err != nil {
				m.toast = "list failed: " + err.Error()
				return m, nil
			}
			if len(sessions) == 0 {
				m.history = append(m.history, api.Message{
					Role:    "system",
					Content: "No saved sessions.",
				})
			} else {
				var b strings.Builder
				b.WriteString("Saved sessions:\n\n")
				for _, s := range sessions {
					label := s.Name
					if s.Title != "" {
						label += " — " + s.Title
					}
					fmt.Fprintf(&b, "- %s (%s, %s, %d messages)\n", label, s.CreatedAt.Format("2006-01-02 15:04"), s.Model, len(s.Messages))
				}
				m.history = append(m.history, api.Message{
					Role:    "system",
					Content: b.String(),
				})
			}
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return m, nil
		}
		if m.modelName == "" {
			m.input.Reset()
			m.lastError = "no model selected — run /model"
			m.refreshTranscript()
			m.viewport.GotoBottom()
			return m, nil
		}
		if cmd := m.submit(); cmd != nil {
			return m, cmd
		}
		return m, nil
	}
	return m, nil
}
