package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

type chatChunkMsg struct {
	gen      int
	content  string
	thinking bool // content is reasoning stream: shown as a live ticker, never stored in history
}
type chatDoneMsg struct {
	gen        int
	content    string
	promptEval int
	evalCount  int
}
type chatErrMsg struct {
	gen int
	err error
}
type chatToolCallsMsg struct {
	gen     int
	content string
	calls   []tools.ToolCall
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

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case faceTickMsg:
		m.faceFrame++
		return m, m.nextFaceTick()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if (m.streaming && m.streamBuf.Len() == 0) || m.pending != nil {
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
			m.toast = "verified ✓ " + msg.label
			cmds = append(cmds, m.endTurnTail()...)
			m.refreshTranscript()
			return m, tea.Batch(cmds...)
		}
		m.verifyAttempts++
		if m.verifyAttempts >= maxVerifyAttempts {
			m.history = append(m.history, api.Message{Role: "system", Content: fmt.Sprintf(
				"[VERIFICATION STILL FAILING after %d attempts] `%s` does not pass:\n\n%s\n\nStop editing. Explain to the user in plain text what is broken and why you couldn't fix it — do not claim it works.",
				m.verifyAttempts, msg.label, msg.output)})
			m.suppressToolsOnce = true
		} else {
			m.history = append(m.history, api.Message{Role: "system", Content: fmt.Sprintf(
				"[VERIFICATION FAILED] You are NOT done — `%s` failed. Read the errors, fix the actual cause (don't blame the tools), then it will be re-checked:\n\n%s",
				msg.label, msg.output)})
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
		if m.state == stateChat {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)

			if m.showNotes {
				m.notesViewport, cmd = m.notesViewport.Update(msg)
				cmds = append(cmds, cmd)
			}
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
			return m, tea.Quit
		}
		if msg.String() == "esc" && m.slashVisible {
			m.slashVisible = false
			m.slashSuggestions = nil
			m.slashSelected = 0
			return m, nil
		}
		if (msg.String() == "ctrl+s" || msg.String() == "esc") && m.streaming && m.stream != nil {
			m.stream.cancel()
			m.turnGen++ // orphan any in-flight stream/tool messages
			m.streaming = false
			m.stream = nil
			m.pending = nil
			m.busySince = time.Time{}
			m.toast = "stopped"
			m.refreshTranscript()
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
		// Shift+Tab: cycle the slash-command menu backwards when it's open.
		if msg.String() == "shift+tab" && m.slashVisible && len(m.slashSuggestions) > 0 {
			n := len(m.slashSuggestions)
			m.slashSelected = (m.slashSelected - 1 + n) % n
			return m, nil
		}
		// Tab: cycle the slash-command menu forwards if open, else cycle mode.
		if msg.String() == "tab" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			if m.slashVisible && len(m.slashSuggestions) > 0 {
				m.slashSelected = (m.slashSelected + 1) % len(m.slashSuggestions)
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
		switch m.state {
		case stateSettings:
			return m.updateSettings(msg)
		case stateModelPicker:
			return m.updatePicker(msg)
		case stateHelp:
			if msg.String() == "esc" || msg.String() == "enter" || msg.String() == "q" {
				m.state = stateChat
				m.input.Focus()
			}
			return m, nil
		case stateNotes:
			if msg.String() == "esc" || msg.String() == "enter" || msg.String() == "q" {
				m.state = stateChat
				m.input.Focus()
			}
			return m, nil
		case statePermission:
			return m.updatePermission(msg)
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
			// Enter accepts the highlighted slash command into the input (so args
			// can be added); a second Enter then runs it.
			if msg.String() == "enter" && m.slashVisible && len(m.slashSuggestions) > 0 {
				m.input.SetValue(m.slashSuggestions[m.slashSelected])
				m.input.CursorEnd()
				m.slashVisible = false
				m.slashSuggestions = nil
				m.slashSelected = 0
				m.layout()
				return m, nil
			}
			if msg.String() == "enter" {
				return m.updateChatKey(msg)
			}
			if msg.String() == "up" && (m.input.Value() == "" || (m.historyIndex < len(m.userHistory) && m.input.Value() == m.userHistory[m.historyIndex])) {
				if len(m.userHistory) > 0 && m.historyIndex > 0 {
					m.historyIndex--
					m.input.SetValue(m.userHistory[m.historyIndex])
					m.input.CursorEnd()
					return m, nil
				}
			}
			if msg.String() == "down" && m.historyIndex < len(m.userHistory) {
				m.historyIndex++
				if m.historyIndex < len(m.userHistory) {
					m.input.SetValue(m.userHistory[m.historyIndex])
					m.input.CursorEnd()
				} else {
					m.input.SetValue("")
				}
				return m, nil
			}
		}

	case modelsLoadedMsg:
		m.models = msg.models
		m.statusMsg = ""
		m.statusErr = false
		m.picker = 0
		// Land the cursor on the just-pulled model if we have one, otherwise on
		// the currently selected model.
		cursorTo := m.cfg.Model
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
		if msg.thinking {
			// Reasoning stream: keep a short tail as a live ticker next to the
			// spinner. Never enters streamBuf, so it never reaches history.
			m.thinkTail += msg.content
			if len(m.thinkTail) > 400 {
				m.thinkTail = m.thinkTail[len(m.thinkTail)-400:]
			}
		} else {
			m.thinkTail = "" // answer started; drop the ticker
			m.streamBuf.WriteString(msg.content)
		}
		if time.Since(m.lastRenderTime) > 60*time.Millisecond || strings.Contains(msg.content, "\n") {
			m.refreshTranscript()
			m.lastRenderTime = time.Now()
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
		}
		if m.stream != nil {
			cmds = append(cmds, m.waitForStream())
		}

	case chatToolCallsMsg:
		if msg.gen != m.turnGen {
			break // stale tool calls from a cancelled/replaced stream
		}
		m.streamRetries = 0 // stream delivered — retry budget refreshes per step
		wasAtBottom := m.viewport.AtBottom()
		preamble := m.streamBuf.String()
		m.streamBuf.Reset()
		calls := dedupeCalls(msg.calls)
		m.history = append(m.history, api.Message{
			Role:      "assistant",
			Content:   preamble,
			ToolCalls: calls,
		})
		m.pending = &pendingBatch{
			calls:   calls,
			results: make([]api.Message, len(calls)),
			started: make([]bool, len(calls)),
			gen:     m.turnGen,
		}
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
				if strings.HasPrefix(msg.result.Content, "error:") {
					m.failedCalls[tools.CallFingerprint(call)]++
				} else if len(tools.MutatedPaths(call.Function.Name, call.Function.Arguments)) > 0 {
					m.turnTouchedFiles = true // a file edit succeeded → verify before finishing
				}
			}
			if msg.modeSwitch != nil && !strings.HasPrefix(msg.result.Content, "error:") {
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

	case chatDoneMsg:
		if msg.gen != m.turnGen {
			break // stale completion from a cancelled/replaced stream
		}
		m.streamRetries = 0
		m.totalTokens = msg.promptEval + msg.evalCount
		wasAtBottom := m.viewport.AtBottom()
		if msg.content != "" {
			m.streamBuf.WriteString(msg.content)
		}
		finalAssistant := m.streamBuf.String()
		m.streamBuf.Reset()
		m.streaming = false
		m.stream = nil
		m.busySince = time.Time{}
		m.lastActivity = time.Now() // idle clock starts when the turn finishes

		// Model-agnostic fallback: some Ollama templates surface tool calls as
		// TEXT instead of via the native channel. If the assistant's message is
		// actually a tool call, route it through the same execution path as a
		// native call (guarded by the step budget so it can't loop forever).
		limit := m.maxSteps
		if m.mode == AutoMode {
			limit = 100
		}
		if parsed := dedupeCalls(m.tools.ParseToolCallsFromContent(finalAssistant)); len(parsed) > 0 && m.stepCount < limit {
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
			m.busySince = time.Now()
			if cmd := m.processPendingTools(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.refreshTranscript()
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
		} else {
			if len(finalAssistant) > 0 {
				m.history = append(m.history, api.Message{
					Role:    "assistant",
					Content: finalAssistant,
				})
			} else {
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
			if m.todos.openCount() > 0 && m.autoContinues < maxAutoContinues && m.stepCount < limit {
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
		m.refreshTranscript()

	case ragLoadedMsg:
		m.applyRagLoaded(msg)

	case ragRefreshedMsg:
		m.applyRagRefreshed(msg)

	case ragRetrievedMsg:
		// Retrieval finished for a user turn; record the block and start the
		// model call now that relevant context is in hand.
		m.lastRagQuery = msg.query
		m.lastRagBlock = msg.block
		cmds = append(cmds, m.startStream())
		m.refreshTranscript()
		m.viewport.GotoBottom()

	case chatErrMsg:
		if msg.gen != m.turnGen {
			break // stale error (e.g. "context canceled" from an esc'd stream)
		}
		// Transient failure (connection reset, 5xx, idle timeout): retry the
		// stream a bounded number of times before killing the turn. History is
		// intact, so the request simply regenerates from the same state.
		if !m.compacting && m.streamRetries < maxStreamRetries {
			m.streamRetries++
			m.logActivity(fmt.Sprintf("stream error, retrying (%d/%d): %v", m.streamRetries, maxStreamRetries, msg.err))
			m.toast = fmt.Sprintf("stream error — retrying (%d/%d)", m.streamRetries, maxStreamRetries)
			m.streamBuf.Reset() // discard the partial response; the retry regenerates it
			cmds = append(cmds, m.startStream())
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
		m.finalizeCheckpoint(m.lastUserMessage())
		m.refreshTranscript()
		m.viewport.GotoBottom()

		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			m.history = append(m.history, api.Message{Role: "user", Content: next})
			m.logActivity("Message (dequeued): " + next)
			m.resetTurnGuards()
			cmds = append(cmds, m.startStream())
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}

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
		var cmd tea.Cmd
		m.urlInput, cmd = m.urlInput.Update(msg)
		cmds = append(cmds, cmd)
		m.keyInput, cmd = m.keyInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateModelPicker:
		var cmd tea.Cmd
		m.pullInput, cmd = m.pullInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateChat:
		prevH := m.input.Height()
		prevSlash := len(m.slashSuggestions)
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		m.updateSlashSuggestions()
		// Relayout when the input grows/shrinks or the slash menu's height changes
		// so the viewport stays sized correctly above the (taller) input area.
		if m.input.Height() != prevH || len(m.slashSuggestions) != prevSlash {
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

func (m *Model) fetchModels() tea.Cmd {
	host := m.host
	return func() tea.Msg {
		list, err := host.GetModelList()
		if err != nil {
			return connectErrMsg{err: err}
		}
		names := make([]string, 0, len(list.Models))
		for _, mod := range list.Models {
			names = append(names, mod.Name)
		}
		return modelsLoadedMsg{models: names}
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
