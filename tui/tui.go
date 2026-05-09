package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

const DefaultHost = "http://localhost:11434"

var (
	accentColor = lipgloss.Color("39")
	textColor   = lipgloss.Color("252")

	titleStyle = func() lipgloss.Style {
		b := lipgloss.RoundedBorder()
		b.Right = "├"
		return lipgloss.NewStyle().
			BorderStyle(b).
			BorderForeground(accentColor).
			Foreground(textColor).
			Padding(0, 1)
	}()

	infoStyle = func() lipgloss.Style {
		b := lipgloss.RoundedBorder()
		b.Left = "┤"
		return titleStyle.BorderStyle(b)
	}()

	borderStyle    = lipgloss.NewStyle().Foreground(accentColor)
	inputBandStyle = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(textColor)
	userStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	assistantStyle = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	hintStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	headingStyle   = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	selectedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Background(accentColor).Bold(true)
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	asciiStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	bodyStyle      = lipgloss.NewStyle().Foreground(textColor)

	modalBg = lipgloss.Color("236")

	modalStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("244")).
			Background(modalBg).
			Foreground(textColor).
			Padding(1, 2)

	modalTitleStyle  = lipgloss.NewStyle().Foreground(textColor).Background(modalBg).Bold(true)
	modalHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(modalBg)
	modalBodyStyle   = lipgloss.NewStyle().Foreground(textColor).Background(modalBg)
	modalMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(modalBg)
	modalAccentStyle = lipgloss.NewStyle().Foreground(accentColor).Background(modalBg).Bold(true)
	modalErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Background(modalBg).Bold(true)
	modalSelectStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Background(lipgloss.Color("215")).Bold(true)
)

const (
	minInputLines = 1
	maxInputLines = 8
	appVersion    = "dev"
)

type state int

const (
	stateSettings state = iota
	stateModelPicker
	stateChat
	stateHelp
)

type config struct {
	Host  string `json:"host"`
	Model string `json:"model,omitempty"`
}

type chatChunkMsg struct{ content string }
type chatDoneMsg struct{}
type chatErrMsg struct{ err error }
type chatToolCallsMsg struct {
	content string
	calls   []mcp.ToolCall
}

type modelsLoadedMsg struct {
	models []string
}
type connectErrMsg struct{ err error }

type streamState struct {
	resp <-chan api.ChatResponse
	errs <-chan error
}

type selection struct {
	active bool
	anchor int // content line index
	cursor int // content line index
}

type Model struct {
	cfg       config
	host      api.OllamaHost
	tools     *mcp.Registry
	state     state
	urlInput  textinput.Model
	models    []string
	picker    int
	modelName string

	history    []api.Message
	transcript *strings.Builder
	viewport   viewport.Model
	input      textarea.Model
	stream     *streamState
	streaming  bool
	pending    *strings.Builder
	statusMsg  string
	statusErr  bool
	lastError  string
	toast      string
	sel        selection

	mdRenderer *glamour.TermRenderer
	mdWidth    int

	width  int
	height int
	ready  bool
}

func New() Model {
	cfg := loadConfig()
	if cfg.Host == "" {
		if env := os.Getenv("OLLAMA_HOST"); env != "" {
			cfg.Host = env
		} else {
			cfg.Host = DefaultHost
		}
	}
	if cfg.Model == "" {
		cfg.Model = os.Getenv("OLLAMA_MODEL")
	}

	host := api.OllamaHost{}
	host.SetURI(cfg.Host)

	ti := textinput.New()
	ti.Prompt = "URL  "
	ti.Placeholder = DefaultHost
	ti.SetValue(cfg.Host)
	ti.Focus()
	ti.SetWidth(60)

	ta := textarea.New()
	ta.Placeholder = "Improve documentation in @filename"
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(minInputLines)
	styles := ta.Styles()
	styles.Focused.Base = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor).Background(lipgloss.Color("238"))
	styles.Focused.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styles.Blurred.Base = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styles.Blurred.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	styles.Blurred.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	styles.Blurred.Text = lipgloss.NewStyle().Foreground(textColor).Background(lipgloss.Color("238"))
	ta.SetStyles(styles)

	km := textarea.DefaultKeyMap()
	km.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "newline"),
	)
	ta.KeyMap = km

	m := Model{
		cfg:        cfg,
		host:       host,
		tools:      mcp.DefaultRegistry(),
		state:      stateChat,
		urlInput:   ti,
		input:      ta,
		modelName:  cfg.Model,
		transcript: &strings.Builder{},
		pending:    &strings.Builder{},
	}
	m.input.Focus()
	return m
}

func (m *Model) refreshTranscript() {
	var b strings.Builder
	if len(m.history) == 0 && !m.streaming && m.lastError == "" {
		b.WriteString(m.welcomePanel())
	} else {
		for _, msg := range m.history {
			switch msg.Role {
			case "user":
				b.WriteString(userStyle.Render("You"))
				b.WriteString("\n")
				b.WriteString(msg.Content)
				b.WriteString("\n\n")
			case "tool":
				b.WriteString(renderToolResult(msg.ToolName, msg.Content))
				b.WriteString("\n\n")
			default:
				if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "" {
					continue
				}
				b.WriteString(assistantStyle.Render(m.activeModelName()))
				b.WriteString("\n")
				if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) != "" {
					b.WriteString(m.renderMarkdown(msg.Content))
					b.WriteString("\n")
				}
				for _, call := range msg.ToolCalls {
					b.WriteString(renderToolCall(call))
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
		if m.streaming {
			b.WriteString(assistantStyle.Render(m.activeModelName()))
			b.WriteString("\n")
			if m.pending.Len() > 0 {
				b.WriteString(m.renderMarkdown(m.pending.String()))
			} else {
				b.WriteString(mutedStyle.Render("…"))
			}
			b.WriteString("\n")
		}
	}
	if m.lastError != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.lastError))
		b.WriteString("\n")
	}
	m.transcript.Reset()
	m.transcript.WriteString(b.String())
	m.viewport.SetContent(m.transcript.String())
	if m.sel.active {
		m.applySelectionHighlight()
	}
}

func renderToolCall(call mcp.ToolCall) string {
	args := strings.TrimSpace(string(call.Function.Arguments))
	if args == "" {
		args = "{}"
	}
	args = truncatePlain(strings.ReplaceAll(args, "\n", " "), 200)
	return mutedStyle.Render("› "+call.Function.Name+" ") + bodyStyle.Render(args)
}

func renderToolResult(name, content string) string {
	const maxLines = 12
	const maxWidth = 200
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	var b strings.Builder
	b.WriteString(mutedStyle.Render("← " + name))
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString(mutedStyle.Render("  " + truncatePlain(line, maxWidth)))
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString(mutedStyle.Render("  …"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	width := m.viewport.Width()
	if width <= 4 {
		width = 80
	}
	wrap := width - 2
	if m.mdRenderer == nil || m.mdWidth != wrap {
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(wrap),
		)
		if err != nil {
			return s
		}
		m.mdRenderer = r
		m.mdWidth = wrap
	}
	out, err := m.mdRenderer.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		if !m.ready {
			m.ready = true
		}
		m.mdRenderer = nil
		m.refreshTranscript()

	case tea.MouseClickMsg:
		if m.state == stateChat && msg.Button == tea.MouseLeft {
			line := m.contentLineAt(msg.Y)
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
			line := m.contentLineAt(msg.Y)
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
			m.viewport.SetHighlights(nil)
		}
		return m, nil

	case tea.MouseWheelMsg:
		if m.state == stateChat {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyPressMsg:
		// Any key clears an active selection
		if m.sel.active {
			m.sel.active = false
			m.viewport.SetHighlights(nil)
		}
		if k := msg.String(); k == "ctrl+c" {
			return m, tea.Quit
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
		case stateChat:
			newM, cmd := m.updateChatKey(msg)
			if nm, ok := newM.(Model); ok {
				m = nm
			}
			if cmd != nil {
				return m, cmd
			}
			if m.state != stateChat {
				return m, nil
			}
		}

	case modelsLoadedMsg:
		m.models = msg.models
		m.statusMsg = ""
		m.statusErr = false
		m.picker = 0
		if m.cfg.Model != "" {
			for i, n := range m.models {
				if n == m.cfg.Model {
					m.picker = i
					break
				}
			}
		}
		m.state = stateModelPicker
		return m, nil

	case connectErrMsg:
		m.statusMsg = msg.err.Error()
		m.statusErr = true
		if m.state == stateSettings {
			m.urlInput.Focus()
		} else {
			m.lastError = fmt.Sprintf("connect failed: %v", msg.err)
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}
		return m, nil

	case chatChunkMsg:
		wasAtBottom := m.viewport.AtBottom()
		m.pending.WriteString(msg.content)
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}
		if m.stream != nil {
			cmds = append(cmds, m.waitForStream())
		}

	case chatToolCallsMsg:
		wasAtBottom := m.viewport.AtBottom()
		// Discard any preamble text the model emitted before the tool call.
		// Storing it in history causes models like granite to repeat it in
		// the post-tool response. Standard tool-calling format expects empty
		// content on tool-calling assistant messages anyway.
		m.pending.Reset()
		m.history = append(m.history, api.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: msg.calls,
		})
		m.runToolCalls(msg.calls)
		cmd := m.startStream()
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}
		cmds = append(cmds, cmd)

	case chatDoneMsg:
		wasAtBottom := m.viewport.AtBottom()
		if m.pending.Len() > 0 {
			m.history = append(m.history, api.Message{
				Role:    "assistant",
				Content: m.pending.String(),
			})
		}
		m.pending.Reset()
		m.streaming = false
		m.stream = nil
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}

	case chatErrMsg:
		m.lastError = fmt.Sprintf("error: %v", msg.err)
		m.streaming = false
		m.stream = nil
		m.refreshTranscript()
		m.viewport.GotoBottom()
	}

	switch m.state {
	case stateSettings:
		var cmd tea.Cmd
		m.urlInput, cmd = m.urlInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateChat:
		prevH := m.input.Height()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		desired := clamp(m.input.LineCount(), minInputLines, maxInputLines)
		if desired != prevH {
			m.input.SetHeight(desired)
			m.layout()
		}
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) updateSettings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = stateChat
		m.input.Focus()
		return m, nil
	case "enter":
		uri := strings.TrimSpace(m.urlInput.Value())
		if uri == "" {
			uri = DefaultHost
		}
		m.cfg.Host = uri
		m.host.SetURI(uri)
		saveConfig(m.cfg)
		m.statusMsg = "connecting…"
		m.statusErr = false
		return m, m.fetchModels()
	}
	var cmd tea.Cmd
	m.urlInput, cmd = m.urlInput.Update(msg)
	return m, cmd
}

func (m Model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	case "enter":
		if len(m.models) == 0 {
			return m, nil
		}
		m.modelName = m.models[m.picker]
		m.cfg.Model = m.modelName
		saveConfig(m.cfg)
		m.state = stateChat
		m.input.Focus()
		m.layout()
		m.refreshTranscript()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m, nil
}

func (m Model) updateChatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if m.streaming {
			return m, nil
		}
		val := strings.TrimSpace(m.input.Value())
		m.toast = ""
		switch val {
		case "/quit", "/exit":
			return m, tea.Quit
		case "/settings":
			m.input.Reset()
			m.state = stateSettings
			m.urlInput.Focus()
			return m, nil
		case "/model", "/models":
			m.input.Reset()
			m.statusMsg = "refreshing…"
			m.statusErr = false
			return m, m.fetchModels()
		case "/clear":
			m.input.Reset()
			m.history = nil
			m.lastError = ""
			m.refreshTranscript()
			m.viewport.GotoTop()
			return m, nil
		case "/help", "/?":
			m.input.Reset()
			m.state = stateHelp
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

func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	// We capture mouse so we can implement drag-selection that scrolls.
	v.MouseMode = tea.MouseModeCellMotion
	if !m.ready {
		v.SetContent("\n  Initializing...")
		return v
	}
	base := m.viewChat()
	switch m.state {
	case stateSettings:
		v.SetContent(m.overlayModal(base, m.settingsModal()))
	case stateModelPicker:
		v.SetContent(m.overlayModal(base, m.pickerModal()))
	case stateHelp:
		v.SetContent(m.overlayModal(base, m.helpModal()))
	default:
		v.SetContent(base)
	}
	return v
}

func (m Model) overlayModal(base, modal string) string {
	if m.width <= 0 || m.height <= 0 {
		return modal
	}
	mw := lipgloss.Width(modal)
	mh := lipgloss.Height(modal)
	col := max(0, (m.width-mw)/2)
	row := max(0, (m.height-mh)/2)
	return overlay(base, modal, col, row)
}

func overlay(bg, fg string, col, row int) string {
	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")
	fgWidth := 0
	for _, l := range fgLines {
		if w := lipgloss.Width(l); w > fgWidth {
			fgWidth = w
		}
	}
	for i, fgLine := range fgLines {
		target := row + i
		if target < 0 || target >= len(bgLines) {
			continue
		}
		bgLine := bgLines[target]
		bgW := lipgloss.Width(bgLine)
		need := col + fgWidth
		if bgW < need {
			bgLine += strings.Repeat(" ", need-bgW)
		}
		left := ansi.Truncate(bgLine, col, "")
		right := ansi.TruncateLeft(bgLine, col+lipgloss.Width(fgLine), "")
		bgLines[target] = left + fgLine + right
	}
	return strings.Join(bgLines, "\n")
}

func (m Model) modalWidth() int {
	if m.width <= 0 {
		return 60
	}
	w := m.width * 3 / 5
	if w < 50 {
		w = 50
	}
	if w > 78 {
		w = 78
	}
	if w > m.width-4 {
		w = m.width - 4
	}
	return w
}

func (m Model) modalHeader(title, hint string, innerW int) string {
	t := modalTitleStyle.Render(title)
	h := modalHintStyle.Render(hint)
	pad := max(1, innerW-lipgloss.Width(t)-lipgloss.Width(h))
	return t + modalBodyStyle.Render(strings.Repeat(" ", pad)) + h
}

func (m Model) settingsModal() string {
	w := m.modalWidth()
	innerW := w - 4
	m.urlInput.SetWidth(innerW - 6)
	var b strings.Builder
	b.WriteString(m.modalHeader("Connection", "esc", innerW))
	b.WriteString("\n\n")
	b.WriteString(modalMutedStyle.Render("URL"))
	b.WriteString("\n")
	b.WriteString(m.urlInput.View())
	b.WriteString("\n\n")
	if m.statusMsg != "" {
		if m.statusErr {
			b.WriteString(modalErrorStyle.Render(truncatePlain(m.statusMsg, innerW)))
		} else {
			b.WriteString(modalMutedStyle.Render(truncatePlain(m.statusMsg, innerW)))
		}
		b.WriteString("\n\n")
	}
	hint := modalMutedStyle.Render("enter ") + modalBodyStyle.Render("connect") +
		modalMutedStyle.Render("   esc ") + modalBodyStyle.Render("cancel")
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
}

func (m Model) pickerModal() string {
	w := m.modalWidth()
	innerW := w - 4
	var b strings.Builder
	b.WriteString(m.modalHeader("Select model", "esc", innerW))
	b.WriteString("\n\n")

	host := strings.TrimPrefix(strings.TrimPrefix(m.cfg.Host, "http://"), "https://")
	b.WriteString(modalMutedStyle.Render(truncatePlain(fmt.Sprintf("on %s", host), innerW)))
	b.WriteString("\n\n")

	if len(m.models) == 0 {
		b.WriteString(modalMutedStyle.Render(truncatePlain("no models installed — `ollama pull <name>`", innerW)))
		b.WriteString("\n")
	} else {
		b.WriteString(modalAccentStyle.Render("Available"))
		b.WriteString("\n")
		view := pickerWindow(len(m.models), m.picker, 8)
		for i := view.start; i < view.end; i++ {
			name := m.models[i]
			marker := "  "
			if name == m.cfg.Model {
				marker = modalAccentStyle.Render(" •")
			}
			row := truncatePlain(name, innerW-4)
			if i == m.picker {
				line := modalSelectStyle.Render(padCell(" "+row+" ", innerW-2))
				b.WriteString(marker + line)
			} else {
				b.WriteString(marker + " " + modalBodyStyle.Render(padCell(row, innerW-3)))
			}
			b.WriteString("\n")
		}
		if view.start > 0 || view.end < len(m.models) {
			b.WriteString(modalMutedStyle.Render(fmt.Sprintf("   %d / %d", m.picker+1, len(m.models))))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	hint := modalMutedStyle.Render("↑↓ ") + modalBodyStyle.Render("select") +
		modalMutedStyle.Render("   enter ") + modalBodyStyle.Render("chat") +
		modalMutedStyle.Render("   r ") + modalBodyStyle.Render("refresh")
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
}

func (m Model) helpModal() string {
	w := m.modalWidth()
	innerW := w - 4
	rows := []struct{ key, desc string }{
		{"", "Slash commands"},
		{"/help", "show this screen"},
		{"/settings", "change Ollama URL"},
		{"/model", "pick a model"},
		{"/clear", "reset the conversation"},
		{"/copy", "copy last response to system clipboard"},
		{"/quit", "exit"},
		{"", ""},
		{"", "Keys"},
		{"enter", "send message"},
		{"shift+enter", "newline in input"},
		{"shift+↑/↓", "scroll one line"},
		{"ctrl+↑/↓", "scroll one line (alt)"},
		{"pgup/pgdn", "page up/down"},
		{"ctrl+u/d", "half page up/down"},
		{"ctrl+c", "quit"},
		{"", ""},
		{"", "Mouse"},
		{"click+drag", "select lines (auto-scrolls at edges)"},
		{"release", "selection is copied to clipboard"},
		{"wheel", "scroll viewport"},
	}

	var b strings.Builder
	b.WriteString(m.modalHeader("Help", "esc", innerW))
	b.WriteString("\n\n")
	keyW := 14
	for _, r := range rows {
		if r.key == "" && r.desc == "" {
			b.WriteString("\n")
			continue
		}
		if r.key == "" {
			b.WriteString(modalAccentStyle.Render(r.desc))
			b.WriteString("\n")
			continue
		}
		k := padCell(r.key, keyW)
		b.WriteString(modalMutedStyle.Render(k))
		b.WriteString(modalBodyStyle.Render(truncatePlain(r.desc, innerW-keyW)))
		b.WriteString("\n")
	}
	hint := modalMutedStyle.Render("esc ") + modalBodyStyle.Render("close")
	b.WriteString("\n")
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
}

type windowRange struct{ start, end int }

func pickerWindow(total, cursor, size int) windowRange {
	if total <= size {
		return windowRange{0, total}
	}
	start := cursor - size/2
	if start < 0 {
		start = 0
	}
	end := start + size
	if end > total {
		end = total
		start = end - size
	}
	return windowRange{start, end}
}

func (m Model) viewChat() string {
	return fmt.Sprintf(
		"%s\n%s\n%s\n%s",
		m.headerView(),
		m.viewport.View(),
		m.inputView(),
		m.footerView(),
	)
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.urlInput.SetWidth(min(m.width-6, 80))
	headerH := lipgloss.Height(m.headerView())
	footerH := lipgloss.Height(m.footerView())
	inputH := m.input.Height() + 1
	vpH := m.height - headerH - footerH - inputH
	if vpH < 1 {
		vpH = 1
	}
	if !m.ready {
		m.viewport = viewport.New(
			viewport.WithWidth(m.width),
			viewport.WithHeight(vpH),
		)
		empty := key.NewBinding(key.WithKeys())
		m.viewport.KeyMap = viewport.KeyMap{
			PageDown:     key.NewBinding(key.WithKeys("pgdown")),
			PageUp:       key.NewBinding(key.WithKeys("pgup")),
			HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
			HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
			Up:           key.NewBinding(key.WithKeys("shift+up", "ctrl+up")),
			Down:         key.NewBinding(key.WithKeys("shift+down", "ctrl+down")),
			Left:         empty,
			Right:        empty,
		}
		m.viewport.HighlightStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("232")).
			Background(lipgloss.Color("215"))
		m.viewport.SelectedHighlightStyle = m.viewport.HighlightStyle
	} else {
		m.viewport.SetWidth(m.width)
		m.viewport.SetHeight(vpH)
	}
	m.input.SetWidth(m.width)
}

func (m Model) fetchModels() tea.Cmd {
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

func (m *Model) submit() tea.Cmd {
	value := strings.TrimRight(m.input.Value(), "\n")
	if strings.TrimSpace(value) == "" {
		return nil
	}

	m.history = append(m.history, api.Message{Role: "user", Content: value})
	m.lastError = ""

	m.input.Reset()
	m.input.SetHeight(minInputLines)
	m.layout()

	cmd := m.startStream()
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return cmd
}

func (m Model) waitForStream() tea.Cmd {
	s := m.stream
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case chunk, ok := <-s.resp:
			if !ok {
				return chatDoneMsg{}
			}
			if len(chunk.Message.ToolCalls) > 0 {
				return chatToolCallsMsg{
					content: chunk.Message.Content,
					calls:   chunk.Message.ToolCalls,
				}
			}
			if chunk.Done {
				if chunk.Message.Content != "" {
					return chatChunkMsg{content: chunk.Message.Content}
				}
				return chatDoneMsg{}
			}
			return chatChunkMsg{content: chunk.Message.Content}
		case err, ok := <-s.errs:
			if !ok || err == nil {
				return chatDoneMsg{}
			}
			return chatErrMsg{err: err}
		}
	}
}

func (m *Model) startStream() tea.Cmd {
	msgs := make([]api.Message, 0, len(m.history)+1)
	msgs = append(msgs, api.Message{Role: "system", Content: systemPrompt})
	msgs = append(msgs, m.history...)
	respCh, errCh := m.host.ContinousChat(api.ChatRequest{
		Model:    m.modelName,
		Messages: msgs,
		Tools:    m.tools.Definitions(),
	})
	m.stream = &streamState{resp: respCh, errs: errCh}
	m.streaming = true
	m.pending.Reset()
	return m.waitForStream()
}

const systemPrompt = `You are a coding assistant in a Linux terminal. You have tools that read and modify the user's real filesystem and run shell commands.

When to use tools:
- The user asks about files, directories, code, or system state -> call tools to find out. Never invent file contents or descriptions from filenames alone.
- "Summarize this codebase" / "what's in X" -> list_directory, then read_file on the real source files before describing them. Chain multiple tools in one turn when needed.
- Prefer dedicated tools when one fits. Use run_shell for anything not covered.

Available tools:
- read_file (supports start_line/end_line for big files), write_file, append_file, edit_file (exact-string replace; preferred for incremental edits), delete_file (set recursive for dirs), move_file, copy_file, touch, make_directory.
- list_directory, find_files (glob by basename, walks with max_depth, skips .git/node_modules), file_info (stat), get_working_directory.
- grep (regex search), run_shell (bash -c for awk/sed/find/pipelines).
- For editing existing files prefer edit_file over write_file — write_file overwrites the whole file.

When NOT to use tools:
- Meta questions about you, your capabilities, or what tools you have -> answer directly from this prompt. Do NOT call list_directory just to "look around".
- Greetings, identity questions, or anything that doesn't depend on the user's filesystem.

Output rules:
- Do NOT write conversational text before a tool call. If you're going to call a tool, emit only the tool call. Save your prose for after you have the tool's result.
- After tools return, produce ONE concise final answer based on what you actually saw. Never restate your own previous text.
- Be terse. No preamble, no "How may I assist you further?" or similar closers. The user is in a terminal and reads quickly.`

func (m *Model) runToolCalls(calls []mcp.ToolCall) {
	for _, call := range calls {
		result, err := m.tools.Invoke(context.Background(), call)
		if err != nil {
			result = fmt.Sprintf("error: %v", err)
		}
		m.history = append(m.history, api.Message{
			Role:     "tool",
			ToolName: call.Function.Name,
			Content:  result,
		})
	}
}

func (m Model) headerView() string {
	title := titleStyle.Render(fmt.Sprintf("ollama · %s · %s", m.activeModelName(), m.cfg.Host))
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(title)
	}
	line := strings.Repeat("─", max(0, width-lipgloss.Width(title)))
	return lipgloss.JoinHorizontal(lipgloss.Center, title, line)
}

func (m Model) footerView() string {
	status := "ready"
	if m.streaming {
		status = "streaming…"
	}
	info := infoStyle.Render(status)
	hintText := " /help · enter send · shift+enter newline · shift+↑/↓ scroll · ctrl+c quit "
	if m.toast != "" {
		hintText = " " + m.toast + " "
	}
	hint := hintStyle.Render(hintText)
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(info) + lipgloss.Width(hint)
	}
	pad := max(0, width-lipgloss.Width(info)-lipgloss.Width(hint))
	return lipgloss.JoinHorizontal(lipgloss.Center, hint, strings.Repeat(" ", pad), info)
}

func (m Model) contentLineAt(screenY int) int {
	headerH := lipgloss.Height(m.headerView())
	viewportY := screenY - headerH
	if viewportY < 0 {
		return -1
	}
	if viewportY >= m.viewport.Height() {
		return -2
	}
	return m.viewport.YOffset() + viewportY
}

func (m *Model) selectionRange() (int, int, []string, bool) {
	if !m.sel.active {
		return 0, 0, nil, false
	}
	content := m.transcript.String()
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return 0, 0, nil, false
	}
	s, e := m.sel.anchor, m.sel.cursor
	if s > e {
		s, e = e, s
	}
	if s < 0 {
		s = 0
	}
	if e >= len(lines) {
		e = len(lines) - 1
	}
	return s, e, lines, true
}

func (m *Model) applySelectionHighlight() {
	s, e, lines, ok := m.selectionRange()
	if !ok {
		m.viewport.SetHighlights(nil)
		return
	}
	startByte := 0
	for i := 0; i < s; i++ {
		startByte += len(lines[i]) + 1
	}
	endByte := startByte
	for i := s; i <= e; i++ {
		endByte += len(lines[i])
		if i < e {
			endByte++
		}
	}
	m.viewport.SetHighlights([][]int{{startByte, endByte}})
}

func (m *Model) copySelection() {
	s, e, lines, ok := m.selectionRange()
	if !ok {
		return
	}
	plain := ansi.Strip(strings.Join(lines[s:e+1], "\n"))
	if strings.TrimSpace(plain) == "" {
		return
	}
	if err := clipboard.WriteAll(plain); err != nil {
		m.toast = fmt.Sprintf("clipboard error: %v", err)
		return
	}
	m.toast = fmt.Sprintf("copied %d chars", len(plain))
}

func lastAssistantMessage(history []api.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			return history[i].Content
		}
	}
	return ""
}

func (m Model) inputView() string {
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(m.input.View())
	}
	status := "ready"
	if m.streaming {
		status = "streaming"
	}
	label := " " + status + " "
	line := mutedStyle.Render("─" + label + strings.Repeat("─", max(0, width-lipgloss.Width(label)-1)))
	input := inputBandStyle.Width(width).Render(m.input.View())
	return line + "\n" + input
}

func (m Model) welcomePanel() string {
	width := 100
	if m.width > 0 {
		width = clamp(m.width-4, 40, 100)
	}

	title := fmt.Sprintf(" Ollama Code %s ", appVersion)
	topFill := max(0, width-lipgloss.Width(title)-3)
	top := borderStyle.Render("╭─") + headingStyle.Render(title) + borderStyle.Render(strings.Repeat("─", topFill)+"╮")
	bottom := borderStyle.Render("╰" + strings.Repeat("─", width-2) + "╯")
	inner := width - 4 // 1 char border + 1 char pad on each side

	rows := []string{""}
	rows = append(rows, centerCell(bodyStyle.Render("Welcome back "+displayName()+"!"), inner))
	rows = append(rows, "")
	rows = append(rows, llamaRows(inner)...)
	rows = append(rows, "")
	rows = append(rows, m.welcomeInfoRows(inner)...)
	rows = append(rows, "")

	var b strings.Builder
	b.WriteString(top)
	b.WriteString("\n")
	for i, row := range rows {
		b.WriteString(borderStyle.Render("│"))
		b.WriteString(" ")
		b.WriteString(padCell(row, inner))
		b.WriteString(" ")
		b.WriteString(borderStyle.Render("│"))
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(bottom)
	return b.String()
}

func (m Model) welcomeInfoRows(width int) []string {
	host := strings.TrimPrefix(strings.TrimPrefix(m.cfg.Host, "http://"), "https://")
	modelLine := "Ask Ollama to explain code, draft tests, or plan a patch"
	if m.modelName == "" {
		modelLine = "Run /model to choose a local model before chatting"
	}
	statusLine := fmt.Sprintf("%s · %s", m.activeModelName(), host)

	rows := []string{
		mutedStyle.Render(truncatePlain(statusLine, width)),
		borderStyle.Render(strings.Repeat("─", width)),
		headingStyle.Render("Tips for getting started"),
		bodyStyle.Render(truncatePlain(modelLine, width)),
		"",
		headingStyle.Render("Recent activity"),
		mutedStyle.Render("No recent activity"),
	}
	return rows
}

func llamaRows(width int) []string {
	lines := strings.Split(strings.Trim(ollamaLlamaSmallASCII, "\n"), "\n")
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		if line == "" {
			rows = append(rows, "")
			continue
		}
		line = cropCenterPlain(line, width)
		rows = append(rows, centerCell(asciiStyle.Render(line), width))
	}
	return rows
}

const ollamaLlamaSmallASCII = `
          @@@@            @@@@
       @@@@@@@@        @@@@@@@@
      @@@@  @@@@      @@@@  @@@@
      @@@    @@@@    @@@@    @@@
       @@@@@@@@@@@@@@@@@@@@@@@@
        @@@@@@@@@@@@@@@@@@@@@@
      @@@@@@@@        @@@@@@@@
    @@@@@@@              @@@@@@@
   @@@@@      @@@@@@@@      @@@@@
  @@@@@     @@@@    @@@@     @@@@@
  @@@@@      @@@@@@@@      @@@@@
   @@@@@@                @@@@@@
     @@@@@@@@@@@@@@@@@@@@@@@@
        @@@@@@@@@@@@@@@@@@
      @@@@              @@@@
     @@@@                @@@@
     @@@@                @@@@
      @@@@              @@@@
`

const ollamaLlamaASCII = `
                     @@@@                                                  @@@@
                  @@@@@@@@@@@                                          @@@@@@@@@@@
                @@@@@@@@@@@@@@@                                      @@@@@@@@@@@@@@@
               @@@@@@@@@@@@@@@@@                                    @@@@@@@@@@@@@@@@@
              @@@@@@@@@@@@@@@@@@@                                  @@@@@@@@@@@@@@@@@@@
             @@@@@@@@@  @@@@@@@@@@                                @@@@@@@@@@  @@@@@@@@@
            @@@@@@@@@    @@@@@@@@@                                @@@@@@@@@    @@@@@@@@@
            @@@@@@@@@     @@@@@@@@@                              @@@@@@@@@     @@@@@@@@@
           @@@@@@@@@       @@@@@@@@                              @@@@@@@@       @@@@@@@@@
           @@@@@@@@@       @@@@@@@@@         @@@@@@@@@@         @@@@@@@@@       @@@@@@@@@
           @@@@@@@@        @@@@@@@@@    @@@@@@@@@@@@@@@@@@@@    @@@@@@@@@        @@@@@@@@
           @@@@@@@@         @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@         @@@@@@@@
          @@@@@@@@@         @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@         @@@@@@@@@
          @@@@@@@@@         @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@         @@@@@@@@@
          @@@@@@@@@         @@@@@@@@@@@@@@@             @@@@@@@@@@@@@@@@         @@@@@@@@@
          @@@@@@@@@         @@@@@@@@@@@@                    @@@@@@@@@@@@         @@@@@@@@@
           @@@@@@@@     @@@@@@@@@@@@@                          @@@@@@@@@@@@@     @@@@@@@@
           @@@@@@@@@@@@@@@@@@@@@@@@                              @@@@@@@@@@@@@@@@@@@@@@@@
           @@@@@@@@@@@@@@@@@@@@@@@                                @@@@@@@@@@@@@@@@@@@@@@@
           @@@@@@@@@@@@@@@@@@@@@@                                  @@@@@@@@@@@@@@@@@@@@@@
         @@@@@@@@@@@@@@@@@@@@@@@                                    @@@@@@@@@@@@@@@@@@@@@@@
       @@@@@@@@@@@@@@                                                          @@@@@@@@@@@@@@
     @@@@@@@@@@@@                                                                  @@@@@@@@@@@@
    @@@@@@@@@@@                                                                      @@@@@@@@@@@
   @@@@@@@@@@                                                                          @@@@@@@@@@
  @@@@@@@@@@                                                                            @@@@@@@@@@
 @@@@@@@@@@                                                                              @@@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@@
@@@@@@@@                                     @@@@@@@@@@                                     @@@@@@@@
@@@@@@@@             @@@@@@@            @@@@@@@@@@@@@@@@@@@@            @@@@@@@             @@@@@@@@
@@@@@@@@            @@@@@@@@@@       @@@@@@@@@@@@@@@@@@@@@@@@@@       @@@@@@@@@@            @@@@@@@@
@@@@@@@@           @@@@@@@@@@@    @@@@@@@@@@@@@@@  @@@@@@@@@@@@@@@    @@@@@@@@@@@           @@@@@@@@
@@@@@@@@@          @@@@@@@@@@@   @@@@@@@@@                @@@@@@@@@   @@@@@@@@@@@          @@@@@@@@@
@@@@@@@@@           @@@@@@@@@  @@@@@@@@                      @@@@@@@@  @@@@@@@@@          @@@@@@@@@
 @@@@@@@@@            @@@@@   @@@@@@@                          @@@@@@@   @@@@@            @@@@@@@@@
  @@@@@@@@@                   @@@@@@         @@@@@@@@@@         @@@@@@                   @@@@@@@@@
   @@@@@@@@@@                 @@@@@@         @@@@@@@@@@          @@@@@                 @@@@@@@@@@
    @@@@@@@@@@               @@@@@@            @@@@@@            @@@@@@               @@@@@@@@@@
    @@@@@@@@@@               @@@@@@            @@@@@@            @@@@@@               @@@@@@@@@@
    @@@@@@@@@                 @@@@@@           @@@@@@           @@@@@@                 @@@@@@@@@
   @@@@@@@@@                  @@@@@@@                          @@@@@@@                  @@@@@@@@@
  @@@@@@@@@                    @@@@@@@@                      @@@@@@@@                    @@@@@@@@@
  @@@@@@@@@                     @@@@@@@@@@@              @@@@@@@@@@@                     @@@@@@@@@
 @@@@@@@@@                        @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@                        @@@@@@@@@
 @@@@@@@@@                           @@@@@@@@@@@@@@@@@@@@@@@@@@                           @@@@@@@@@
 @@@@@@@@                                 @@@@@@@@@@@@@@@@                                 @@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@@
@@@@@@@@@                                                                                  @@@@@@@@@
 @@@@@@@@                                                                                  @@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
  @@@@@@@@@                                                                              @@@@@@@@@
   @@@@@@@@@                                                                            @@@@@@@@@
    @@@@@@@@@@                                                                        @@@@@@@@@@
     @@@@@@@@@@                                                                       @@@@@@@@@
     @@@@@@@@@                                                                        @@@@@@@@@
    @@@@@@@@@@                                                                         @@@@@@@@@
   @@@@@@@@@                                                                            @@@@@@@@@
   @@@@@@@@@                                                                            @@@@@@@@@
  @@@@@@@@@                                                                              @@@@@@@@@
  @@@@@@@@@                                                                              @@@@@@@@@
  @@@@@@@@                                                                                @@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
 @@@@@@@@@                                                                                @@@@@@@@@
  @@@@@@@@                                                                                @@@@@@@@
  @@@@@@@@@                                                                              @@@@@@@@@
   @@@@@@@@                                                                              @@@@@@@@@
`

func (m Model) activeModelName() string {
	if strings.TrimSpace(m.modelName) == "" {
		return "no model selected"
	}
	return m.modelName
}

func displayName() string {
	name := strings.TrimSpace(os.Getenv("USER"))
	if name == "" {
		return "there"
	}
	return name
}

func padCell(s string, width int) string {
	if lipgloss.Width(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-lipgloss.Width(s))
}

func centerCell(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	left := (width - w) / 2
	right := width - w - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

func cropCenterPlain(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if width >= len(runes) {
		return s
	}
	start := (len(runes) - width) / 2
	return string(runes[start : start+width])
}

func truncatePlain(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:min(len(s), width)]
	}
	runes := []rune(s)
	var b strings.Builder
	for _, r := range runes {
		if lipgloss.Width(b.String()+string(r)+"...") > width {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString("...")
	return b.String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ollama_code", "config.json")
}

func loadConfig() config {
	var c config
	path := configPath()
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveConfig(c config) {
	path := configPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func Run() error {
	p := tea.NewProgram(New())
	_, err := p.Run()
	return err
}
