package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/atotto/clipboard"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/companion"
	"github.com/javanhut/ollama_code/internal/session"
	"github.com/javanhut/ollama_code/tools"
)

func (m *Model) updateSettings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = stateChat
		m.input.Focus()
		return m, nil
	case "tab", "shift+tab":
		m.toggleSettingsFocus()
		return m, nil
	case "enter":
		uri := strings.TrimSpace(m.urlInput.Value())
		if uri == "" {
			uri = DefaultHost
		}
		m.cfg.Host = uri
		m.cfg.APIKey = strings.TrimSpace(m.keyInput.Value())
		m.host.SetURI(uri)
		m.host.SetAPIKey(resolveAPIKey(m.cfg))
		saveConfig(m.cfg)
		m.statusMsg = "connecting…"
		m.statusErr = false
		return m, m.fetchModels()
	}
	var cmd tea.Cmd
	if m.settingsFocus == settingsFocusKey {
		m.keyInput, cmd = m.keyInput.Update(msg)
	} else {
		m.urlInput, cmd = m.urlInput.Update(msg)
	}
	return m, cmd
}

// toggleSettingsFocus moves focus between the URL and API-key fields in the
// connection modal.
func (m *Model) toggleSettingsFocus() {
	if m.settingsFocus == settingsFocusURL {
		m.settingsFocus = settingsFocusKey
		m.urlInput.Blur()
		m.keyInput.Focus()
	} else {
		m.settingsFocus = settingsFocusURL
		m.keyInput.Blur()
		m.urlInput.Focus()
	}
}

// focusSettings (re)focuses the URL field when the connection modal opens so
// both inputs start in a known state.
func (m *Model) focusSettings() {
	m.settingsFocus = settingsFocusURL
	m.keyInput.Blur()
	m.urlInput.Focus()
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
	case "esc":
		m.state = stateChat
		m.input.Focus()
		return m, nil
	case "r":
		m.statusMsg = "refreshing…"
		m.statusErr = false
		return m, m.fetchModels()
	case "p":
		m.pullErr = ""
		return m, m.pullInput.Focus()
	case "enter":
		if len(m.models) == 0 {
			return m, nil
		}
		m.modelName = m.models[m.picker]
		m.cfg.Model = m.modelName
		saveConfig(m.cfg)
		m.resolveProfile()
		m.state = stateChat
		m.input.Focus()
		m.layout()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m, nil
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
	m.modelName = name
	m.cfg.Model = name
	saveConfig(m.cfg)
	m.resolveProfile()
	m.toast = "default model set to " + name
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
	fmt.Fprintf(&b, "- tools: %t · thinking: %t\n", p.SupportsTools, p.SupportsThinking)
	temp := "ollama default"
	if p.Temperature != nil {
		temp = fmt.Sprintf("%.2f", *p.Temperature)
	} else if p.smallModel() {
		temp = "0.20 (auto: small model)"
	}
	fmt.Fprintf(&b, "- temperature: %s\n", temp)
	if p.TopP != nil {
		fmt.Fprintf(&b, "- top_p: %.2f\n", *p.TopP)
	}
	if p.NumPredict != nil {
		fmt.Fprintf(&b, "- num_predict: %d\n", *p.NumPredict)
	}
	b.WriteString("Change with: /model use <name> · /model ctx <tokens> · /model temp <value> — or /models to list, switch, and pull.")
	m.history = append(m.history, api.Message{Role: "system", Content: b.String()})
	m.refreshTranscript()
	m.viewport.GotoBottom()
}

// cancelPull aborts an in-flight model download and clears the streaming state.

func (m *Model) updatePermission(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		m.state = stateChat
		return m, nil
	}
	switch msg.String() {
	case "y", "enter":
		i := m.pending.index
		call := m.pending.calls[i]
		m.pending.started[i] = true
		m.state = stateChat
		return m, m.invokeToolCmd(m.pending.gen, i, call)
	case "a":
		m.pending.allowAll = true
		m.state = stateChat
		return m, m.processPendingTools()
	case "n", "esc":
		i := m.pending.index
		call := m.pending.calls[i]
		// Count the denial as a failure of this exact call so the identical-call
		// short-circuit in processPendingTools fires on a retry, and record it in
		// recentCalls so oscillation detection can see reject-retry loops.
		fp := tools.CallFingerprint(call)
		m.failedCalls[fp]++
		m.recentCalls = append(m.recentCalls, fp)
		if len(m.recentCalls) > recentCallsKept {
			m.recentCalls = m.recentCalls[len(m.recentCalls)-recentCallsKept:]
		}
		m.pending.results[i] = api.Message{
			Role:     "tool",
			ToolName: call.Function.Name,
			Content:  "denied by user. Do NOT retry this call or a minor variant of it — the user rejected it. Take a different approach, or ask the user how to proceed in plain text.",
		}
		m.pending.started[i] = true
		m.pending.done++
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
			m.toast = fmt.Sprintf("queued (%d in queue)", len(m.queue))
			return m, nil
		}

		m.slashVisible = false
		m.slashSuggestions = nil
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
			m.modelInfoCommand(strings.TrimSpace(strings.TrimPrefix(val, "/model")))
			return m, nil
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
		case "/settings":
			m.input.Reset()
			m.state = stateSettings
			m.urlInput.SetValue(m.cfg.Host)
			m.keyInput.SetValue(m.cfg.APIKey)
			m.focusSettings()
			return m, nil
		case "/models":
			m.input.Reset()
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
			m.historyIndex = len(m.userHistory)
			m.lastError = ""
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
		case "/archive":
			m.input.Reset()
			if m.kvStore == nil {
				m.toast = "archive not initialized"
				return m, nil
			}
			// Just show the most recent archive for demo
			var lastKey string
			for k := range m.kvStore.GetFullData() {
				if strings.HasPrefix(k, "archive_") {
					if k > lastKey {
						lastKey = k
					}
				}
			}
			if lastKey == "" {
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
		case "/save":
			m.input.Reset()
			name := strings.TrimSpace(strings.TrimPrefix(val, "/save"))
			if name == "" {
				name = time.Now().Format("2006-01-02_15-04-05")
			}
			s := session.Session{
				Name:      name,
				CreatedAt: time.Now(),
				Model:     m.modelName,
				Mode:      m.mode.String(),
				Notes:     m.notes.get(),
				Messages:  append([]api.Message(nil), m.history...),
			}
			if err := session.Save(s); err != nil {
				m.toast = "save failed: " + err.Error()
			} else {
				m.toast = "saved session '" + name + "'"
			}
			return m, nil
		case "/load":
			m.input.Reset()
			name := strings.TrimSpace(strings.TrimPrefix(val, "/load"))
			if name == "" {
				m.toast = "usage: /load <name>"
				return m, nil
			}
			s, err := session.Load(name)
			if err != nil {
				m.toast = "load failed: " + err.Error()
				return m, nil
			}
			m.history = append([]api.Message(nil), s.Messages...)
			m.notes.set(s.Notes)
			m.modelName = s.Model
			if s.Mode != "" {
				switch s.Mode {
				case "explore":
					m.mode = ExploreMode
				case "plan":
					m.mode = PlanMode
				case "write":
					m.mode = WriteMode
				case "auto":
					m.mode = AutoMode
				}
			}
			m.cfg.Model = m.modelName
			saveConfig(m.cfg)
			m.resolveProfile()
			m.refreshTranscript()
			m.viewport.GotoBottom()
			m.toast = "loaded session '" + name + "'"
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
					fmt.Fprintf(&b, "- %s (%s, %s, %d messages)\n", s.Name, s.CreatedAt.Format("2006-01-02 15:04"), s.Model, len(s.Messages))
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
