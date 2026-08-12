package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

type chatChunkMsg struct {
	gen      int
	content  string // answer text
	thinking string // reasoning text: isolated from content and never stored in history
}
type streamRenderMsg struct{ gen int }
type chatDoneMsg struct {
	gen        int
	content    string
	thinking   string
	promptEval int
	evalCount  int
}
type chatErrMsg struct {
	gen int
	err error
}

// retryStreamMsg fires after a retry backoff delay to re-kick a failed stream;
// gen guards against stale retries from a cancelled or replaced turn.
type retryStreamMsg struct{ gen int }
type chatToolCallsMsg struct {
	gen        int
	content    string
	thinking   string
	calls      []tools.ToolCall
	promptEval int
	evalCount  int
}

type toolResultMsg struct {
	gen        int
	index      int
	result     api.Message
	modeSwitch *modeSwitchRequest
}

type compactDoneMsg struct {
	summary string
	index   int
}

type modelsLoadedMsg struct {
	models []string
	// from names the provider the list came from ("" = default host). Selecting
	// a model has to remember this or the name is stored bare and every unbound
	// mode then asks the LOCAL daemon for a model only that provider has.
	from string
}

// modelsAutoMsg carries the model list fetched at startup so the first available
// model can be auto-loaded when none is configured.
type modelsAutoMsg struct {
	models []string
}
type connectErrMsg struct{ err error }

type companionTranscriptMsg struct{ text string }
type companionErrorMsg struct{ err error }
type companionStoppedMsg struct{}

type pullStreamState struct {
	prog   <-chan api.PullProgress
	errs   <-chan error
	cancel context.CancelFunc
	model  string
}

type pullProgressMsg struct{ p api.PullProgress }
type pullDoneMsg struct {
	model string
	err   error
}

// droppedToolCallNotice tells the model a call it made on a deliberately
// tool-less request was not executed, so it stops waiting for a result that is
// never coming. Shared by both channels a call can arrive through — native and
// parsed-from-text — because a withholding enforced on only one of them is a
// suggestion on the other.
func droppedToolCallNotice(calls []tools.ToolCall) string {
	name := batchSingleTool(calls)
	if name == "" {
		name = "tool"
	}
	return fmt.Sprintf("[TOOL CALL DROPPED] Tools were disabled for that message, so your %s call was NOT executed — nothing ran and no result is coming. Reply in plain text: answer with what you already know, or state your blocker.", name)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case calibrationDoneMsg:
		if msg.err != nil {
			m.toast = "calibration failed: " + msg.err.Error()
			return m, nil
		}
		m.lastCalibration = &msg.result
		m.toast = fmt.Sprintf("calibration recommends %s (%.0f%% correct); apply with /model calibrate apply", msg.result.Recommended, msg.result.Score()*100)
		return m, nil
	case faceTickMsg:
		m.faceFrame++
		return m, m.nextFaceTick()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Keep the phase spinners and elapsed counters moving during any busy
		// phase, including stalls mid-stream and the turn-start/verify gates.
		if m.streaming || m.pending != nil || m.verifying || m.retrieving || m.compacting {
			m.refreshTranscript()
		}
		if dc := m.maybeDream(); dc != nil {
			return m, tea.Batch(cmd, dc)
		}
		return m, cmd

	case dreamDoneMsg:
		m.applyDream(msg)
		return m, nil

	case verifyDoneMsg:
		m.verifying = false
		if msg.ok {
			m.lastVerification = fmt.Sprintf("%s (`%s`, checkpoint %s)", msg.label, msg.command, msg.fingerprint)
			if m.profile.reviewPass() && !m.reviewedThisTurn {
				m.reviewedThisTurn = true
				m.history = append(m.history, api.Message{Role: "system", Content: fmt.Sprintf("[ADVERSARIAL REVIEW] Verification passed: %s. Before finishing, inspect the actual diff as a skeptical reviewer. Look for incorrect assumptions, missing edge cases, unsafe behavior, and inadequate tests. If you find a real issue, fix it and verify again. If not, state that the review found no blocking issue and finish.", m.lastVerification)})
				m.busySince = time.Now()
				m.refreshTranscript()
				return m, m.startStream()
			}
			m.toast = "verified ✓ " + m.lastVerification
			if msg.lint != "" {
				m.toast += fmt.Sprintf(" · %d lint findings in trace", strings.Count(msg.lint, "\n")+1)
			}
			cmds = append(cmds, m.endTurnTail()...)
			m.refreshTranscript()
			return m, tea.Batch(cmds...)
		}
		m.verifyAttempts++
		if m.verifyAttempts >= maxVerifyAttempts {
			m.history = append(m.history, api.Message{Role: "system", Content: fmt.Sprintf(
				"[VERIFICATION STILL FAILING after %d attempts] `%s` does not pass:\n\n%s\n\nStop editing. Explain to the user in plain text what is broken and why you couldn't fix it — do not claim it works.",
				m.verifyAttempts, msg.label, repairDetail(msg.output, msg.lint))})
			m.suppressToolsOnce = true
		} else {
			m.history = append(m.history, api.Message{Role: "system", Content: fmt.Sprintf(
				"[VERIFICATION FAILED] You are NOT done — `%s` failed. Read the errors, fix the actual cause (don't blame the tools), then it will be re-checked:\n\n%s",
				msg.label, repairDetail(msg.output, msg.lint))})
		}
		m.busySince = time.Now()
		cmds = append(cmds, m.startStream())
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return m, tea.Batch(cmds...)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		if !m.ready {
			m.ready = true
		}
		m.md.reset()
		m.notesMd.reset()
		m.refreshTranscript()

	case tea.MouseClickMsg:
		if m.state == stateChat && msg.Button == tea.MouseLeft {
			line := m.contentLineAt(msg.X, msg.Y)
			if line >= 0 {
				m.toast = ""
				m.sel = selection{active: true, anchor: line, cursor: line}
				m.applySelectionHighlight()
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if m.state == stateChat && m.sel.active && msg.Button == tea.MouseLeft {
			headerH := lipgloss.Height(m.headerView())
			topY := headerH
			botY := headerH + m.viewport.Height() - 1
			if msg.Y <= topY && m.viewport.YOffset() > 0 {
				m.viewport.ScrollUp(1)
			} else if msg.Y >= botY {
				m.viewport.ScrollDown(1)
			}
			line := m.contentLineAt(msg.X, msg.Y)
			switch line {
			case -1:
				m.sel.cursor = m.viewport.YOffset()
			case -2:
				m.sel.cursor = m.viewport.YOffset() + m.viewport.Height() - 1
			default:
				m.sel.cursor = line
			}
			m.applySelectionHighlight()
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if m.state == stateChat && m.sel.active {
			m.copySelection()
			m.sel.active = false
			m.viewport.ClearHighlights()
		}
		return m, nil

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		switch m.state {
		case stateChat:
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)

			if m.showNotes {
				m.notesViewport, cmd = m.notesViewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		case stateHelp:
			m.helpViewport, cmd = m.helpViewport.Update(msg)
			cmds = append(cmds, cmd)
		case stateDiff:
			m.diffViewport, cmd = m.diffViewport.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyPressMsg:
		// Any key counts as activity and wakes a sleeping/dreaming session.
		m.lastActivity = time.Now()
		m.faceLastKey = time.Now()
		if m.asleep || m.dreaming {
			m.wake()
		}
		// Any key clears an active selection
		if m.sel.active {
			m.sel.active = false
			m.viewport.ClearHighlights()
		}
		if k := msg.String(); k == "ctrl+c" {
			// Mid-turn, ctrl+c interrupts the turn (like esc) instead of
			// quitting; it only quits when idle.
			if m.streaming {
				m.cancelSubagents()
				return m, m.interruptTurn()
			}
			return m, tea.Quit
		}
		if msg.String() == "esc" && m.slashVisible {
			m.dismissSlash()
			return m, nil
		}
		if msg.String() == "esc" && m.mentionVisible {
			m.dismissMention()
			return m, nil
		}
		// Not at a permission prompt: there, esc means "deny this call" and is
		// handled by updatePermission, not by cancelling the whole turn.
		if (msg.String() == "ctrl+s" || msg.String() == "esc") && m.streaming && m.stream != nil && m.state != statePermission {
			m.cancelSubagents()
			return m, m.interruptTurn()
		}
		// Idle esc with no menus open cancels running background sub-agents —
		// the only thing still working when no turn is in flight.
		if msg.String() == "esc" && !m.streaming && m.pending == nil && m.state == stateChat && m.runningSubagentJobs() > 0 {
			m.cancelSubagents()
			m.toast = "cancelled background sub-agents"
			return m, nil
		}
		if msg.String() == "ctrl+t" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			m.expandTools = !m.expandTools
			if m.expandTools {
				m.toast = "tool calls expanded"
			} else {
				m.toast = "tool calls collapsed"
			}
			m.refreshTranscript()
			return m, nil
		}
		// Search takes every key while its prompt is focused.
		if m.search.active && m.state == stateChat {
			return m.updateSearch(msg)
		}
		if msg.String() == "ctrl+f" && m.state == stateChat {
			m.openSearch()
			m.layout()
			return m, nil
		}
		// n/N step through matches once the prompt is dismissed but hits remain.
		if m.state == stateChat && len(m.search.matches) > 0 && m.input.Value() == "" {
			switch msg.String() {
			case "n":
				m.stepMatch(1)
				return m, nil
			case "N":
				m.stepMatch(-1)
				return m, nil
			case "esc":
				m.closeSearch(false)
				m.layout()
				return m, nil
			}
		}
		// Jump back to the live end of the transcript (the scroll cue names it).
		if msg.String() == "ctrl+g" && m.state == stateChat {
			m.viewport.GotoBottom()
			m.layout()
			return m, nil
		}
		if msg.String() == "ctrl+o" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			m.cfg.Verbose = !m.cfg.Verbose
			saveConfig(m.cfg)
			if m.cfg.Verbose {
				m.toast = "verbose mode on"
			} else {
				m.toast = "verbose mode off"
			}
			m.refreshTranscript()
			return m, nil
		}
		// Shift+Tab: cycle the slash-command menu backwards when it's open,
		// otherwise cycle the workflow mode.
		if msg.String() == "shift+tab" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			if m.slashVisible && len(m.slashSuggestions) > 0 {
				n := len(m.slashSuggestions)
				m.slashSelected = (m.slashSelected - 1 + n) % n
				return m, nil
			}
			if m.mentionVisible && len(m.mentionSuggestions) > 0 {
				n := len(m.mentionSuggestions)
				m.mentionSelected = (m.mentionSelected - 1 + n) % n
				return m, nil
			}
			changed := m.applyModeTransition(m.mode.next(), "")
			if changed {
				m.refreshTranscript()
				m.viewport.GotoBottom()
			}
			m.layout()
			return m, nil
		}
		// Tab completes to the highlighted entry — not the next one. ↑/↓ move the
		// highlight (see the stateChat branch below).
		if msg.String() == "tab" && m.slashVisible && len(m.slashSuggestions) > 0 {
			m.input.SetValue(m.slashSuggestions[m.slashSelected])
			m.input.CursorEnd()
			m.dismissSlash()
			m.layout()
			return m, nil
		}
		if msg.String() == "tab" && m.mentionVisible && len(m.mentionSuggestions) > 0 {
			m.acceptMention()
			return m, nil
		}
		switch m.state {
		case stateSettings:
			return m.updateSettings(msg)
		case stateModelPicker:
			return m.updatePicker(msg)
		case stateHelp:
			switch msg.String() {
			case "esc", "enter", "q":
				m.state = stateChat
				m.input.Focus()
				return m, nil
			}
			var cmd tea.Cmd
			m.helpViewport, cmd = m.helpViewport.Update(msg)
			return m, cmd
		case stateNotes, stateStats:
			if msg.String() == "esc" || msg.String() == "enter" || msg.String() == "q" {
				m.state = stateChat
				m.input.Focus()
			}
			return m, nil
		case statePermission:
			return m.updatePermission(msg)
		case stateRouteConfirm:
			return m.updateRouteConfirm(msg)
		case stateDiff:
			switch msg.String() {
			case "esc", "q", "enter":
				m.state = stateChat
				m.input.Focus()
				return m, nil
			}
			var cmd tea.Cmd
			m.diffViewport, cmd = m.diffViewport.Update(msg)
			return m, cmd
		case stateChat:
			// ↑/↓ move the highlight while the menu is open, ahead of history recall.
			if m.slashVisible && len(m.slashSuggestions) > 0 {
				n := len(m.slashSuggestions)
				switch msg.String() {
				case "down":
					m.slashSelected = (m.slashSelected + 1) % n
					return m, nil
				case "up":
					m.slashSelected = (m.slashSelected - 1 + n) % n
					return m, nil
				}
			}
			if m.mentionVisible && len(m.mentionSuggestions) > 0 {
				n := len(m.mentionSuggestions)
				switch msg.String() {
				case "down":
					m.mentionSelected = (m.mentionSelected + 1) % n
					return m, nil
				case "up":
					m.mentionSelected = (m.mentionSelected - 1 + n) % n
					return m, nil
				}
			}
			// Enter accepts the highlighted slash command into the input (so args
			// can be added); a second Enter then runs it. A fully-typed command wins
			// over the menu — "/model" runs /model, not the suggested "/models".
			if msg.String() == "enter" && m.slashVisible && len(m.slashSuggestions) > 0 &&
				!isSlashCommand(strings.TrimSpace(m.input.Value())) {
				m.input.SetValue(m.slashSuggestions[m.slashSelected])
				m.input.CursorEnd()
				m.dismissSlash()
				m.layout()
				return m, nil
			}
			// Same accept-first rhythm for @file completion: Enter fills in the
			// highlighted path, a second Enter submits.
			if msg.String() == "enter" && m.mentionVisible && len(m.mentionSuggestions) > 0 {
				m.acceptMention()
				return m, nil
			}
			if msg.String() == "enter" {
				return m.updateChatKey(msg)
			}
			// History recall only fires when the cursor is on the first/last
			// *visual* row and the buffer is untouched; otherwise up/down are
			// ordinary cursor movement inside a multi-line message. Without the
			// row check, arrowing through a wrapped message silently replaced it.
			if msg.String() == "up" && m.atFirstVisualRow() && m.inputIsHistory() {
				if len(m.userHistory) > 0 && m.historyIndex > 0 {
					m.historyIndex--
					m.input.SetValue(m.userHistory[m.historyIndex])
					m.input.CursorEnd()
					m.layout()
					return m, nil
				}
			}
			if msg.String() == "down" && m.atLastVisualRow() && m.inputIsHistory() && m.historyIndex < len(m.userHistory) {
				m.historyIndex++
				if m.historyIndex < len(m.userHistory) {
					m.input.SetValue(m.userHistory[m.historyIndex])
					m.input.CursorEnd()
				} else {
					m.input.SetValue("")
				}
				m.layout()
				return m, nil
			}
		}

	case modelsLoadedMsg:
		m.models = msg.models
		m.modelsFrom = msg.from
		m.statusMsg = ""
		m.statusErr = false
		m.picker = 0
		// Land the cursor on the just-pulled model if we have one, otherwise on
		// the currently selected model.
		_, cursorTo := m.splitRouteSpec(m.cfg.Model)
		if m.pullSelect != "" {
			cursorTo = m.pullSelect
			m.pullSelect = ""
		}
		if cursorTo != "" {
			for i, n := range m.models {
				if n == cursorTo {
					m.picker = i
					break
				}
			}
		}
		m.state = stateModelPicker
		return m, nil

	case modelsAutoMsg:
		m.models = msg.models
		// Auto-load the first available model if none is configured yet.
		if strings.TrimSpace(m.modelName) == "" && len(msg.models) > 0 {
			m.modelName = msg.models[0]
			m.cfg.Model = m.modelName
			saveConfig(m.cfg)
			m.resolveProfile()
			m.toast = "loaded model " + m.modelName
			m.refreshTranscript()
		}
		return m, nil

	case pullProgressMsg:
		if s := strings.TrimSpace(msg.p.Status); s != "" {
			m.pullStatus = s
		}
		if msg.p.Total > 0 {
			m.pullTotal = msg.p.Total
			m.pullCompleted = msg.p.Completed
		}
		if m.pullStream != nil {
			cmds = append(cmds, m.waitForPull())
		}

	case pullDoneMsg:
		m.pulling = false
		if m.pullStream != nil && m.pullStream.cancel != nil {
			m.pullStream.cancel()
		}
		m.pullStream = nil
		m.pullStatus = ""
		m.pullCompleted, m.pullTotal = 0, 0
		if msg.err != nil {
			m.pullErr = msg.err.Error()
			return m, nil
		}
		// Success: refresh the list and drop the cursor on the new model.
		m.pullErr = ""
		m.pullName = ""
		m.toast = "pulled " + msg.model
		m.pullSelect = msg.model
		return m, m.fetchModels()

	case connectErrMsg:
		m.statusMsg = msg.err.Error()
		m.statusErr = true
		if m.state == stateSettings {
			if m.settingsFocus == settingsFocusKey {
				m.keyInput.Focus()
			} else {
				m.urlInput.Focus()
			}
		} else {
			m.lastError = fmt.Sprintf("connect failed: %v", msg.err)
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}
		return m, nil

	case chatChunkMsg:
		if msg.gen != m.turnGen {
			break // stale chunk from a cancelled/replaced stream
		}
		wasAtBottom := m.viewport.AtBottom()
		if msg.thinking != "" {
			// Reasoning stream: keep a short tail as a live ticker next to the
			// spinner. Never enters streamBuf, so it never reaches history.
			m.thinkTail += msg.thinking
			if len(m.thinkTail) > 400 {
				m.thinkTail = m.thinkTail[len(m.thinkTail)-400:]
			}
			m.recordThinking(msg.thinking)
			m.streamThinking.WriteString(msg.thinking)
		}
		if msg.content != "" {
			m.thinkTail = "" // answer started; drop the ticker
			m.streamBuf.WriteString(msg.content)
		}
		// Paint at a responsive cadence independently of token boundaries. When a
		// chunk arrives inside the cadence window, schedule the missing frame: the
		// old opportunistic throttle simply skipped it and waited for some later
		// token (or completion) to happen to trigger another paint.
		elapsed := time.Since(m.lastRenderTime)
		if m.lastRenderTime.IsZero() || elapsed >= streamRenderInterval {
			m.refreshTranscript()
			m.lastRenderTime = time.Now()
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
		} else if !m.renderQueued {
			m.renderQueued = true
			gen := m.turnGen
			cmds = append(cmds, tea.Tick(streamRenderInterval-elapsed, func(time.Time) tea.Msg {
				return streamRenderMsg{gen: gen}
			}))
		}
		if m.stream != nil {
			cmds = append(cmds, m.waitForStream())
		}

	case streamRenderMsg:
		if msg.gen != m.turnGen {
			break
		}
		m.renderQueued = false
		if !m.streaming {
			break
		}
		wasAtBottom := m.viewport.AtBottom()
		m.refreshTranscript()
		m.lastRenderTime = time.Now()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}

	case chatToolCallsMsg:
		if msg.gen != m.turnGen {
			break // stale tool calls from a cancelled/replaced stream
		}
		m.streamRetries = 0 // stream delivered — retry budget refreshes per step
		wasAtBottom := m.viewport.AtBottom()
		if msg.thinking != "" {
			m.recordThinking(msg.thinking)
			m.streamThinking.WriteString(msg.thinking)
		}
		preamble := m.streamBuf.String() + msg.content
		m.streamBuf.Reset()
		if msg.promptEval+msg.evalCount > 0 {
			m.totalTokens = msg.promptEval + msg.evalCount
		}
		// Tools withheld by a loop guard stay withheld on this channel too. Native
		// Ollama doesn't parse calls a request never advertised, but a provider that
		// parses them regardless would walk straight past the text-form drop below
		// and keep a stopped turn running. Hand the reply to the normal end-of-turn
		// path with the calls gone, so the answer still reaches the user.
		if m.stream != nil && m.stream.toolsSuppressed {
			m.history = append(m.history, api.Message{Role: "system", Content: droppedToolCallNotice(msg.calls)})
			return m.Update(chatDoneMsg{gen: msg.gen, content: preamble, promptEval: msg.promptEval, evalCount: msg.evalCount})
		}
		m.recordModelResponse(msg.gen, preamble, msg.calls, msg.promptEval, msg.evalCount)
		calls := dedupeCalls(msg.calls)
		if m.trace != nil && len(calls) != len(msg.calls) {
			_ = m.trace.Record(tracepkg.Event{Kind: "tool_calls_deduplicated", Turn: msg.gen, Model: m.modelName,
				Metadata: map[string]any{"received": len(msg.calls), "kept": len(calls), "calls": msg.calls}})
		}
		m.history = append(m.history, api.Message{
			Role:      "assistant",
			Content:   preamble,
			ToolCalls: calls,
		})
		// Preamble echo guard: a stuck model re-announces the same intent in
		// slightly different words before every tool call. Call it out once;
		// if it keeps going, force a plain-text answer.
		if warn, stopEcho := m.observePreamble(preamble); warn {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[REPEATING YOURSELF] That message restates your previous one. Do not re-announce or rephrase your intent — if you need information, call the tool; if you already have the answer, give it. No narration before tool calls.",
			})
		} else if stopEcho {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[LOOP BROKEN] You keep restating the same message. Tools are disabled for your next response — answer the user in plain text.",
			})
			m.suppressToolsOnce = true
		}
		m.pending = &pendingBatch{
			calls:   calls,
			results: make([]api.Message, len(calls)),
			started: make([]bool, len(calls)),
			gen:     m.turnGen,
		}
		m.markToolsStart(len(calls))
		m.busySince = time.Now()
		cmd := m.processPendingTools()
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case toolResultMsg:
		wasAtBottom := m.viewport.AtBottom()
		// Tool goroutines run on context.Background() and are never cancelled when
		// the pending batch is swapped (cancel via esc/ctrl+s, or a new turn). A
		// straggler from an old batch must be dropped entirely: an out-of-bounds
		// index would panic, and an in-bounds one would silently corrupt the new
		// batch (bogus result + double-counted done). The gen check catches both.
		if m.pending != nil && msg.gen == m.pending.gen && msg.index < len(m.pending.results) {
			m.pending.results[msg.index] = msg.result
			m.pending.done++
			if msg.index < len(m.pending.calls) {
				call := m.pending.calls[msg.index]
				if !tools.ToolResultOK(msg.result.Content) {
					m.failedCalls[tools.CallFingerprint(call)]++
				} else if mutated := tools.MutatedPaths(call.Function.Name, call.Function.Arguments); len(mutated) > 0 {
					m.turnTouchedFiles = true // a file edit succeeded → verify before finishing
					if m.turnChangedPaths == nil {
						m.turnChangedPaths = map[string]bool{}
					}
					for _, path := range mutated {
						m.turnChangedPaths[filepath.Clean(path)] = true
					}
					m.forgetReads(mutated) // re-reading a just-changed file is legitimate
				} else if call.Function.Name == "spawn_subagent" && !subagentCallIsAsync(call) && (m.mode == WriteMode || m.mode == AutoMode) {
					// A synchronous delegated writer uses the same registry but is
					// not checkpointed call-by-call. Conservatively run the
					// repository-wide verification gate even when its final report
					// says it only inspected files. (Background jobs arm the gate
					// on completion instead — see subagentDoneMsg.)
					m.turnTouchedFiles = true
				}
			}
			if msg.modeSwitch != nil && tools.ToolResultOK(msg.result.Content) {
				m.applyModeTransition(msg.modeSwitch.target, msg.modeSwitch.reason)
				m.layout()
			}

			// Update notes viewport in case the tool modified them
			notesText := m.notes.get()
			if notesText == "" {
				notesText = "(empty)"
			}
			m.notesViewport.SetContent(m.renderNotesMarkdown(notesText, m.notesViewport.Width()))

			cmd := m.processPendingTools()
			m.refreshTranscript()
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case subagentDoneMsg:
		// Re-arm the parked waiter first, so the next completion is picked up.
		cmds = append(cmds, m.awaitSubagentEvent())
		job := msg.job
		if job == nil {
			break
		}
		// Deliver the result: the notification carries the full report(s), so
		// the parent collects results by reading the conversation, not by
		// polling (a sub-agent report is bounded final text, unlike a bg shell
		// job's unbounded stream, which is why shell_output exists but no
		// subagent_result tool does).
		m.history = append(m.history, api.Message{Role: "system", Content: job.notification()})
		if job.wasInterrupted() {
			m.toast = fmt.Sprintf("sub-agent job %d cancelled", job.id)
		} else {
			m.toast = fmt.Sprintf("sub-agent job %d finished", job.id)
		}
		// A background writer's edits land after its spawn tool result, so the
		// conservative spawn-time mark can't see them. If the job mutated files
		// (observed via the Before hook), arm the verification gate now.
		if _, mutated, _ := job.snapshot(); mutated && (m.mode == WriteMode || m.mode == AutoMode) {
			m.turnTouchedFiles = true
		}
		m.refreshTranscript()
		m.viewport.GotoBottom()
		// Wake the parent when it is idle so it reacts to the report now
		// instead of waiting for the user's next message. A user-interrupted
		// job doesn't wake: esc meant stop.
		if !job.wasInterrupted() && !m.streaming && m.pending == nil &&
			!m.verifying && !m.retrieving && !m.compacting &&
			m.state == stateChat && m.modelName != "" {
			// The user's turn already ended, so this reply is a new one: it must not
			// inherit that turn's bans, forced ending, or spent step budget — the
			// woken model never ran the loop that earned them. The verification arm
			// above is about the sub-agent's own edits and survives the reset.
			touchedFiles := m.turnTouchedFiles
			m.resetTurnGuards()
			m.turnTouchedFiles = touchedFiles
			cmds = append(cmds, m.startStream())
			m.refreshTranscript()
		}

	case chatDoneMsg:
		if msg.gen != m.turnGen {
			break // stale completion from a cancelled/replaced stream
		}
		m.streamRetries = 0
		// A successful completion supersedes any "stream error — retrying"
		// toast left over from a recovered transient failure.
		if strings.HasPrefix(m.toast, "stream error") {
			m.toast = ""
		}
		m.totalTokens = msg.promptEval + msg.evalCount
		wasAtBottom := m.viewport.AtBottom()
		if msg.thinking != "" {
			m.recordThinking(msg.thinking)
			m.streamThinking.WriteString(msg.thinking)
		}
		if msg.content != "" {
			m.streamBuf.WriteString(msg.content)
		}
		finalAssistant := m.streamBuf.String()
		m.recordModelResponse(msg.gen, finalAssistant, nil, msg.promptEval, msg.evalCount)
		m.streamBuf.Reset()
		// Remember whether this stream was schema-constrained before clearing
		// the stream state: a constrained reply that isn't a tool call is the
		// prose escape envelope and gets unwrapped below.
		constrained := m.stream != nil && m.stream.constrained
		suppressed := m.stream != nil && m.stream.toolsSuppressed
		m.streaming = false
		m.stream = nil
		m.busySince = time.Time{}
		m.lastActivity = time.Now() // idle clock starts when the turn finishes

		// Model-agnostic fallback: some Ollama templates surface tool calls as
		// TEXT instead of via the native channel. If the assistant's message is
		// actually a tool call, route it through the same execution path as a
		// native call (guarded by the step budget so it can't loop forever).
		limit := m.turnStepLimit()
		if m.mode == AutoMode {
			limit = 100
		}
		parsedRaw := m.tools.ParseToolCallsFromContent(finalAssistant)
		parsed := dedupeCalls(parsedRaw)
		// Tools withheld by a loop guard stay withheld. Executing a call parsed
		// back out of the reply's TEXT made every "tools are disabled for your
		// next message" a suggestion — five rounds of the 40-round burn went out
		// with zero tools and ran todo_write anyway. Drop the call, keep its JSON
		// out of the transcript (it is not an answer), and tell the model nothing
		// ran so it stops waiting on a result. Deliberately no re-invoke here:
		// the end-of-turn path below already owns every bounded retry there is
		// (open-todo nudge, citation gate, verify gate), so the drop can never
		// become a loop of its own.
		dropped := suppressed && len(parsed) > 0
		if dropped {
			if m.trace != nil {
				_ = m.trace.Record(tracepkg.Event{Kind: "tool_calls_dropped_suppressed", Turn: msg.gen, Model: m.modelName,
					Metadata: map[string]any{"dropped": len(parsed), "calls": parsedRaw}})
			}
			m.history = append(m.history, api.Message{Role: "system", Content: droppedToolCallNotice(parsed)})
			// Drop the call, keep the answer that came with it. A reply is often a
			// finished answer plus one self-check call, and blanking the whole
			// message discarded the corrected answer the citation gate asked for.
			parsed, finalAssistant = nil, tools.StripToolCalls(finalAssistant)
		}
		if len(parsed) > 0 && m.stepCount < limit {
			if m.trace != nil {
				_ = m.trace.Record(tracepkg.Event{Kind: "tool_calls_parsed_from_content", Turn: msg.gen, Model: m.modelName,
					Metadata: map[string]any{"received": len(parsedRaw), "kept": len(parsed), "calls": parsedRaw}})
			}
			m.history = append(m.history, api.Message{
				Role:      "assistant",
				Content:   finalAssistant,
				ToolCalls: parsed,
			})
			m.pending = &pendingBatch{
				calls:   parsed,
				results: make([]api.Message, len(parsed)),
				started: make([]bool, len(parsed)),
				gen:     m.turnGen,
			}
			m.markToolsStart(len(parsed))
			m.busySince = time.Now()
			if cmd := m.processPendingTools(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.refreshTranscript()
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
		} else {
			// The model chose the schema's prose escape branch: surface the
			// answer text, not the {"response": ...} envelope it arrived in.
			if constrained {
				if prose, ok := agent.UnwrapConstrainedProse(finalAssistant); ok {
					finalAssistant = prose
				}
			}
			if len(finalAssistant) > 0 {
				m.history = append(m.history, api.Message{
					Role:    "assistant",
					Content: finalAssistant,
				})
			} else if !dropped {
				// A dropped tool call is not an interrupted stream: the reply
				// arrived intact, it just wasn't allowed to be a tool call, and
				// the system message above already says so in the transcript.
				m.lastError = "model returned empty response — stream may have been interrupted"
				m.logActivity("WARNING: empty model response (stream ended with no content)")
			}
			m.refreshTranscript()
			if m.companion != nil && finalAssistant != "" {
				_ = m.companion.Speak(finalAssistant)
			}
			if wasAtBottom {
				m.viewport.GotoBottom()
			}

			// Persistent loop: don't let the turn end while the model still has
			// open todos — nudge it to keep going. Bounded by maxAutoContinues (and
			// the step budget) so a model that won't finish can't spin forever.
			// Exempt: a turn stopped for stagnation. Open todos are precisely what
			// the stalled model kept rewriting, so nudging it there re-opens the loop.
			if !m.endTurnAfterReply && m.todos.openCount() > 0 && m.autoContinues < maxAutoContinues && m.stepCount < limit {
				m.autoContinues++
				m.history = append(m.history, api.Message{
					Role: "system",
					Content: fmt.Sprintf("[CONTINUE] %d todo item(s) are still open:\n%s\nKeep working — take the next item now, and mark items completed via todo_write as you finish them. Only stop when every item is completed, or state your blocker explicitly.",
						m.todos.openCount(), m.todos.openSummary()),
				})
				cmds = append(cmds, m.startStream())
				m.refreshTranscript()
				break
			}

			// An offloaded planner speaks no tool protocol, so it can neither
			// record its plan nor call switch_mode. Do both for it and hand
			// straight over to the local model — otherwise the turn dead-ends in
			// plan mode waiting for a shift+tab.
			if m.handOffOffloadedPlan(finalAssistant) {
				cmds = append(cmds, m.startStream())
				m.refreshTranscript()
				m.viewport.GotoBottom()
				break
			}

			// Verified-citations gate (explore mode): an answer that makes code
			// claims must back them with path:line citations that resolve
			// against the workspace. Missing/invalid citations earn one
			// corrective nudge and a re-invoke; the retried answer passes
			// through here again and is accepted as-is, so this can't loop.
			if cc := m.maybeCitationGate(finalAssistant); cc != nil {
				cmds = append(cmds, cc)
				m.refreshTranscript()
				m.viewport.GotoBottom()
				break
			}

			// Turn complete: bank any file changes as one undoable checkpoint.
			m.finalizeCheckpoint(m.lastUserMessage())

			// Verification gate: if this turn edited files, don't let it end on
			// broken code — run a compile check (or challenge the model to prove
			// it verified). On failure this re-invokes the model to keep fixing.
			if vc := m.maybeVerifyGate(); vc != nil {
				cmds = append(cmds, vc)
				m.refreshTranscript()
			} else {
				cmds = append(cmds, m.endTurnTail()...)
			}
		}

	case compactDoneMsg:
		m.compacting = false
		m.toast = "context compacted"
		// Store the summary in the volatile tail (archiveSummary) and DROP the
		// compacted messages, rather than prepending a system message into
		// history. This keeps m.history append-only so the KV-cache prefix
		// (systemPrompt + unchanged history) never shifts.
		idx := min(msg.index, len(m.history))
		m.archiveSummary = msg.summary
		m.history = append([]api.Message(nil), m.history[idx:]...)
		m.rebaseTurnTimes(idx)
		m.refreshTranscript()

	case ragLoadedMsg:
		m.applyRagLoaded(msg)

	case ragRefreshedMsg:
		m.applyRagRefreshed(msg)

	case ragRetrievedMsg:
		// Retrieval finished for a user turn; record the block and start the
		// model call now that relevant context is in hand.
		m.retrieving = false
		m.lastRagQuery = msg.query
		m.lastRagBlock = msg.block
		cmds = append(cmds, m.startStream())
		m.refreshTranscript()
		m.viewport.GotoBottom()

	case chatErrMsg:
		if msg.gen != m.turnGen {
			break // stale error (e.g. "context canceled" from an esc'd stream)
		}
		if m.trace != nil {
			metadata := map[string]any{
				"retry": m.streamRetries, "steps": m.stepCount,
				"partial_content": m.streamBuf.String(), "partial_thinking": m.streamThinking.String(),
			}
			if m.stream != nil {
				metadata["constrained"] = m.stream.constrained
				metadata["source"] = m.stream.modelSource
			}
			_ = m.trace.Record(tracepkg.Event{Kind: "stream_error", Turn: msg.gen, Model: m.modelName, Error: msg.err.Error(), Metadata: metadata})
		}
		// A 400 against a schema-constrained request is the host refusing the
		// format, not a transient failure: step down the fallback ladder and
		// retry immediately rather than burning a stream retry (and its backoff)
		// on a request shape that will deterministically fail again.
		if m.stream != nil && m.stream.constrained && agent.IsFormatRejection(msg.err) && m.downgradeToolCallFormat() {
			m.streamBuf.Reset() // discard any partial response; the retry regenerates it
			m.logActivity(fmt.Sprintf("constrained decoding rejected, trying weaker format: %v", msg.err))
			m.toast = "host rejected constrained decoding — retrying with a weaker format"
			gen := m.turnGen
			cmds = append(cmds, func() tea.Msg { return retryStreamMsg{gen: gen} })
			m.refreshTranscript()
			break
		}
		// Transient failure (connection reset, 5xx, idle timeout): retry the
		// stream a bounded number of times before killing the turn, with a
		// linear backoff so a struggling backend gets room to recover. History
		// is intact, so the request simply regenerates from the same state.
		if !m.compacting && m.streamRetries < maxStreamRetries {
			m.streamRetries++
			delay := time.Duration(m.streamRetries) * 2 * time.Second
			m.logActivity(fmt.Sprintf("stream error, retrying (%d/%d): %v", m.streamRetries, maxStreamRetries, msg.err))
			m.toast = fmt.Sprintf("stream error — retrying (%d/%d) in %ds…", m.streamRetries, maxStreamRetries, int(delay.Seconds()))
			m.streamBuf.Reset() // discard the partial response; the retry regenerates it
			gen := m.turnGen
			cmds = append(cmds, tea.Tick(delay, func(time.Time) tea.Msg { return retryStreamMsg{gen: gen} }))
			m.refreshTranscript()
			break
		}
		source := "local"
		if strings.Contains(m.host.URL(), "ollama.com") {
			source = "cloud"
		}
		m.lastError = fmt.Sprintf("[%s] error: %v", source, msg.err)
		m.streaming = false
		m.stream = nil
		m.compacting = false
		m.busySince = time.Time{}
		m.finishTurnClock()
		m.finalizeCheckpoint(m.lastUserMessage())
		if m.trace != nil {
			_ = m.trace.Record(tracepkg.Event{Kind: "turn_end", Turn: msg.gen, Model: m.modelName,
				Metadata: map[string]any{"reason": "error", "steps": m.stepCount, "open_todos": m.todos.openCount()}})
		}
		m.refreshTranscript()
		m.viewport.GotoBottom()

		if len(m.queue) > 0 {
			cmds = append(cmds, m.dequeueNext())
		}

	case retryStreamMsg:
		if msg.gen != m.turnGen {
			break // stale retry from a cancelled/replaced turn
		}
		cmds = append(cmds, m.startStream())
		m.refreshTranscript()

	case companionTranscriptMsg:
		if m.state == stateChat {
			text := strings.TrimSpace(msg.text)
			if text != "" {
				cur := m.input.Value()
				if cur != "" && !strings.HasSuffix(cur, " ") {
					m.input.InsertString(" ")
				}
				m.input.InsertString(text)
				m.input.Focus()

				// Auto-submit: when the user stops talking, send the message.
				val := strings.TrimSpace(m.input.Value())
				if val != "" {
					if m.streaming {
						m.queue = append(m.queue, val)
						m.input.Reset()
						m.toast = fmt.Sprintf("queued (%d in queue)", len(m.queue))
					} else if m.modelName == "" {
						m.input.Reset()
						m.lastError = "no model selected — run /model"
						m.refreshTranscript()
						m.viewport.GotoBottom()
					} else if cmd := m.submit(); cmd != nil {
						return m, cmd
					}
				}
			}
		}

	case companionErrorMsg:
		m.toast = "companion: " + msg.err.Error()

	case companionStoppedMsg:
		if m.companion != nil {
			_ = m.companion.Close()
			m.companion = nil
		}
		m.toast = "companion exited"
	}

	switch m.state {
	case stateSettings:
		// Every field, not just the two the modal started with: a bracketed paste
		// arrives as tea.PasteMsg, which the type switch above doesn't match, so
		// this tail is the only thing that delivers it. Each input ignores the
		// message unless it holds focus.
		var cmd tea.Cmd
		m.urlInput, cmd = m.urlInput.Update(msg)
		cmds = append(cmds, cmd)
		m.keyInput, cmd = m.keyInput.Update(msg)
		cmds = append(cmds, cmd)
		m.nameInput, cmd = m.nameInput.Update(msg)
		cmds = append(cmds, cmd)
		m.envInput, cmd = m.envInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateModelPicker:
		var cmd tea.Cmd
		m.pullInput, cmd = m.pullInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateChat:
		prevBandH := lipgloss.Height(m.inputView())
		prevSlash := len(m.slashSuggestions)
		prevMention := len(m.mentionSuggestions)
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		m.updateSlashSuggestions()
		m.updateMentionSuggestions()
		// Relayout when the rendered input band grows/shrinks (wrapped pastes,
		// slash menu, narrow-mode status line) so the viewport stays sized
		// correctly above the input area. The rendered height — not just the
		// textarea's own Height() — is what actually takes screen rows.
		if lipgloss.Height(m.inputView()) != prevBandH ||
			len(m.slashSuggestions) != prevSlash || len(m.mentionSuggestions) != prevMention {
			m.layout()
		}
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)

		if m.showNotes {
			m.notesViewport, cmd = m.notesViewport.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) cancelPull() {
	if m.pullStream != nil && m.pullStream.cancel != nil {
		m.pullStream.cancel()
	}
	m.pullStream = nil
	m.pulling = false
	m.pullStatus = ""
}

// startPull kicks off a streaming /api/pull and returns the first pump command.
func (m *Model) startPull(name string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	progCh, errCh := m.host.PullModel(ctx, name)
	m.pullStream = &pullStreamState{prog: progCh, errs: errCh, cancel: cancel, model: name}
	m.pulling = true
	m.pullName = name
	m.pullStatus = "starting…"
	m.pullErr = ""
	m.pullCompleted, m.pullTotal = 0, 0
	return m.waitForPull()
}

// waitForPull reads one progress update (or completion) from the pull stream.
func (m *Model) waitForPull() tea.Cmd {
	s := m.pullStream
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case p, ok := <-s.prog:
			if !ok {
				return pullDoneMsg{model: s.model}
			}
			return pullProgressMsg{p: p}
		case err, ok := <-s.errs:
			if !ok || err == nil {
				return pullDoneMsg{model: s.model}
			}
			return pullDoneMsg{model: s.model, err: err}
		case <-time.After(pullIdleTimeout):
			if s.cancel != nil {
				s.cancel()
			}
			return pullDoneMsg{model: s.model, err: fmt.Errorf("pull idle timeout after %s", pullIdleTimeout)}
		}
	}
}

func (m *Model) fetchModels() tea.Cmd { return m.fetchModelsFrom(m.host, m.activeProvider()) }

// fetchModelsFrom lists models from a specific endpoint, so the settings modal
// can verify the endpoint it just saved rather than whichever one is routed.
// from is carried through to the picker so a selection keeps its provider.
func (m *Model) fetchModelsFrom(host api.OllamaHost, from string) tea.Cmd {
	return func() tea.Msg {
		list, err := host.GetModelList()
		if err != nil {
			return connectErrMsg{err: err}
		}
		names := make([]string, 0, len(list.Models))
		for _, mod := range list.Models {
			names = append(names, mod.Name)
		}
		return modelsLoadedMsg{models: names, from: from}
	}
}

// autoLoadModels fetches the model list at startup so the first available model
// can be selected automatically. Connection errors are swallowed (returns an
// empty list) so the app simply stays in its "no model loaded" state.
func (m *Model) autoLoadModels() tea.Cmd {
	host := m.host
	return func() tea.Msg {
		list, err := host.GetModelList()
		if err != nil {
			return modelsAutoMsg{}
		}
		names := make([]string, 0, len(list.Models))
		for _, mod := range list.Models {
			names = append(names, mod.Name)
		}
		return modelsAutoMsg{models: names}
	}
}

// lastUserMessage returns the text of the most recent user message, truncated
// for use as a checkpoint label.
// lastTurnDiffs collects the unified diffs from the most recent assistant turn's
// tool results (walking back to the previous user message), in chronological
// order. Returns "" when the last turn changed no files.
