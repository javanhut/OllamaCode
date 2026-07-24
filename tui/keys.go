package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/atotto/clipboard"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/companion"
	"github.com/javanhut/ollama_code/internal/session"
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
		m.pending.results[i] = api.Message{
			Role:     "tool",
			ToolName: call.Function.Name,
			Content:  "denied by user",
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
		case "/model", "/models":
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
