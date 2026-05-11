package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
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
	"github.com/javanhut/ollama_code/internal/huffman"
	"github.com/javanhut/ollama_code/internal/storage"
)

const DefaultHost = "http://localhost:11434"

var (
	accentColor = lipgloss.Color("205") // Soft Pink
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
	maxInputLines = 20
	appVersion    = "dev"
)

type state int

const (
	stateSettings state = iota
	stateModelPicker
	stateChat
	stateHelp
	statePermission
	stateNotes
)

type Mode int

const (
	ExploreMode Mode = iota
	PlanMode
	WriteMode
)

func (m Mode) String() string {
	switch m {
	case ExploreMode:
		return "explore"
	case PlanMode:
		return "plan"
	case WriteMode:
		return "write"
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
	}
	return lipgloss.Color("39")
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
	"read_user_memory":      true,
	"update_user_memory":    true,
	"web_fetch":             true,
	"web_search":            true,
	"get_project_tree":      true,
	"find_symbol":           true,
	"ask_user":              true,
	"git_status":            true,
	"git_diff":              true,
	"git_log":               true,
	"hash_file":             true,
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
	"apply_diff":     true,
	"git_add":        true,
	"git_commit":     true,
}

type config struct {
	Host     string   `json:"host"`
	Model    string   `json:"model,omitempty"`
	Activity []string `json:"activity,omitempty"`
	Verbose  bool     `json:"verbose,omitempty"`
}

func (m *Model) logActivity(s string) {
	m.cfg.Activity = append([]string{s}, m.cfg.Activity...)
	if len(m.cfg.Activity) > 5 {
		m.cfg.Activity = m.cfg.Activity[:5]
	}
	saveConfig(m.cfg)
}

type chatChunkMsg struct{ content string }
type chatDoneMsg struct {
	promptEval int
	evalCount  int
}
type chatErrMsg struct{ err error }
type chatToolCallsMsg struct {
	content string
	calls   []mcp.ToolCall
}

type toolResultMsg struct {
	index  int
	result api.Message
}

type compactDoneMsg struct {
	summary string
	index   int
}

type modelsLoadedMsg struct {
	models []string
}
type connectErrMsg struct{ err error }

type streamState struct {
	resp   <-chan api.ChatResponse
	errs   <-chan error
	cancel context.CancelFunc
}

type selection struct {
	active bool
	anchor int // content line index
	cursor int // content line index
}

type pendingBatch struct {
	calls    []mcp.ToolCall
	results  []api.Message
	started  []bool
	done     int
	index    int
	allowAll bool
}

const notesFile = ".ollama_notes.md"

type sessionNotes struct {
	mu   sync.Mutex
	text string
}

func (n *sessionNotes) load() {
	n.mu.Lock()
	defer n.mu.Unlock()
	data, err := os.ReadFile(notesFile)
	if err == nil {
		n.text = string(data)
	}
}

func (n *sessionNotes) save() {
	_ = os.WriteFile(notesFile, []byte(n.text), 0o644)
}

func (n *sessionNotes) get() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.text
}

func (n *sessionNotes) set(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.text = s
	n.save()
}

func (n *sessionNotes) appendLine(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.text != "" && !strings.HasSuffix(n.text, "\n") {
		n.text += "\n"
	}
	n.text += s
	n.save()
}

type Model struct {
	cfg       config
	host      api.OllamaHost
	tools     *mcp.Registry
	notes     *sessionNotes
	mode      Mode
	state     state
	urlInput  textinput.Model
	models    []string
	picker    int
	modelName string
	pending   *pendingBatch

	history    []api.Message
	transcript *strings.Builder
	viewport   viewport.Model
	input      textarea.Model
	stream     *streamState
	streaming  bool
	streamBuf  *strings.Builder
	statusMsg  string
	statusErr  bool
	lastError  string
	toast      string
	sel        selection

	mdRenderer *glamour.TermRenderer
	mdWidth    int

	notesViewport viewport.Model
	spinner       spinner.Model
	gitBranch     string
	queue         []string

	showNotes bool
	width     int
	height    int
	ready     bool

	totalTokens  int
	contextLimit int
	kvStore      *storage.KVStore
	userMemory   *storage.KVStore
	mdCache      map[string]string
}

func New() *Model {
	cfg := loadConfig()
	// ... (host setup ...)
	host := api.OllamaHost{}
	host.SetURI(cfg.Host)

	archivePath := filepath.Join(os.Getenv("HOME"), ".ollama_code", "archive.json")
	memoryPath := filepath.Join(os.Getenv("HOME"), ".ollama_code", "user_memory.json")
	
	kv, _ := storage.NewKVStore(archivePath)
	um, _ := storage.NewKVStore(memoryPath)

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
	styles.Focused.Base = lipgloss.NewStyle().Background(lipgloss.Color("238")).Padding(0, 2)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor).Background(lipgloss.Color("238"))
	styles.Focused.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styles.Blurred.Base = lipgloss.NewStyle().Background(lipgloss.Color("238")).Padding(0, 2)
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

	notes := &sessionNotes{}
	notes.load()
	registry := mcp.DefaultRegistry()
	registry.Register(readNotesTool(notes))
	registry.Register(updateNotesTool(notes))
	registry.Register(appendNotesTool(notes))
	registry.Register(readUserMemoryTool(um))
	registry.Register(updateUserMemoryTool(um))

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m := &Model{
		cfg:        cfg,
		host:       host,
		tools:      registry,
		notes:      notes,
		mode:       ExploreMode,
		state:      stateChat,
		urlInput:   ti,
		input:      ta,
		modelName:  cfg.Model,
		spinner:    s,
		gitBranch:  getGitBranch(),
		transcript: &strings.Builder{},
		streamBuf:  &strings.Builder{},
		contextLimit: 124000,
		kvStore:    kv,
		userMemory: um,
		mdCache:    make(map[string]string),
	}
	m.input.Focus()
	return m
}

func getGitBranch() string {
	cmd := exec.Command("git", "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (m *Model) refreshTranscript() {
	var b strings.Builder
	if len(m.history) == 0 && !m.streaming && m.lastError == "" {
		b.WriteString(m.welcomePanel())
	} else {
		consumed := make(map[int]bool)
		for i := 0; i < len(m.history); i++ {
			if consumed[i] {
				continue
			}
			msg := m.history[i]
			switch msg.Role {
			case "user":
				b.WriteString(userStyle.Render("You"))
				b.WriteString("\n")
				b.WriteString(msg.Content)
				b.WriteString("\n\n")
			case "tool":
				b.WriteString(m.renderMarkdown(renderToolResult(msg.ToolName, msg.Content, m.cfg.Verbose), true))
				b.WriteString("\n\n")
			case "assistant":
				b.WriteString(assistantStyle.Copy().Foreground(m.mode.color()).Render(m.activeModelName()))
				b.WriteString("\n")
				
				if msg.Content != "" {
					b.WriteString(m.renderMarkdown(msg.Content, true))
					b.WriteString("\n")
				}
				
				if len(msg.ToolCalls) > 0 {
					for _, call := range msg.ToolCalls {
						var result *api.Message
						for j := i + 1; j < len(m.history); j++ {
							if !consumed[j] && m.history[j].Role == "tool" && m.history[j].ToolName == call.Function.Name {
								result = &m.history[j]
								consumed[j] = true
								break
							}
						}
						
						if result != nil {
							b.WriteString(m.renderMarkdown(renderCollapsedTool(call, result.Content, m.cfg.Verbose), true))
						} else {
							b.WriteString(m.renderMarkdown(renderToolCall(call, m.cfg.Verbose), true))
						}
						b.WriteString("\n")
					}
				}
				b.WriteString("\n")
			default:
				if msg.Content != "" {
					b.WriteString(m.renderMarkdown(msg.Content, true))
					b.WriteString("\n\n")
				}
			}
		}
		// ... streaming etc ...

		if m.streaming {
			b.WriteString(assistantStyle.Copy().Foreground(m.mode.color()).Render(m.activeModelName()))
			b.WriteString("\n")
			if m.streamBuf.Len() > 0 {
				b.WriteString(m.renderMarkdown(m.streamBuf.String(), false))
			} else {
				b.WriteString(m.spinner.View() + mutedStyle.Render(" Thinking..."))
			}
			b.WriteString("\n")
		}
	}
	// ... rest of method ...
	if m.lastError != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render(m.lastError))
		b.WriteString("\n")
	}

	content := b.String()
	if content != m.transcript.String() {
		m.transcript.Reset()
		m.transcript.WriteString(content)
		m.viewport.SetContent(content)
	}
	
	if m.sel.active {
		m.applySelectionHighlight()
	}
}

func renderCollapsedTool(call mcp.ToolCall, content string, verbose bool) string {
	status := "completed"
	if strings.HasPrefix(content, "error:") {
		status = "failed"
	}
	
	header := fmt.Sprintf("**›** `%s` (%s)", call.Function.Name, status)
	if !verbose {
		return header
	}

	const maxLines = 12
	const maxWidth = 200
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString("> " + truncatePlain(line, maxWidth))
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("> …")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderToolCall(call mcp.ToolCall, verbose bool) string {
	name := fmt.Sprintf("**›** `%s`", call.Function.Name)
	if !verbose {
		return name
	}
	args := strings.TrimSpace(string(call.Function.Arguments))
	if args == "" {
		args = "{}"
	}
	args = truncatePlain(strings.ReplaceAll(args, "\n", " "), 200)
	return name + " " + args
}

func renderToolResult(name, content string, verbose bool) string {
	status := "completed"
	if strings.HasPrefix(content, "error:") {
		status = "failed"
	}
	
	header := fmt.Sprintf("**←** `%s` (%s)", name, status)
	if !verbose {
		return header
	}

	const maxLines = 12
	const maxWidth = 200
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString("> " + truncatePlain(line, maxWidth))
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("> …")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) renderMarkdown(s string, useCache bool) string {
	if strings.TrimSpace(s) == "" {
		return s
	}

	if useCache {
		if cached, ok := m.mdCache[s]; ok {
			return cached
		}
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
		// Invalidate cache if width changes
		m.mdCache = make(map[string]string)
	}
	out, err := m.mdRenderer.Render(s)
	if err != nil {
		return s
	}
	res := strings.TrimRight(out, "\n")
	if useCache {
		m.mdCache[s] = res
	}
	return res
}

func (m *Model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		if !m.ready {
			m.ready = true
		}
		m.mdRenderer = nil
		m.mdCache = make(map[string]string)
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

			if m.showNotes {
				m.notesViewport, cmd = m.notesViewport.Update(msg)
				cmds = append(cmds, cmd)
			}
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
		if (msg.String() == "ctrl+s" || msg.String() == "esc") && m.streaming && m.stream != nil {
			m.stream.cancel()
			m.toast = "stopped"
			return m, nil
		}
		// Tab cycles mode regardless of which state we're in (chat/help/notes).
		if msg.String() == "tab" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			oldMode := m.mode
			m.mode = m.mode.next()

			// Update textarea prompt color
			st := m.input.Styles()
			st.Focused.Prompt = st.Focused.Prompt.Foreground(m.mode.color())
			m.input.SetStyles(st)

			m.toast = fmt.Sprintf("mode: %s (%s)", m.mode, m.mode.hint())

			if oldMode == PlanMode && m.mode == WriteMode {
				notes := m.notes.get()
				if notes != "" {
					m.history = append(m.history, api.Message{
						Role:    "system",
						Content: "Plan Summary from Session Notes:\n\n" + notes,
					})
					m.refreshTranscript()
					m.viewport.GotoBottom()
				}
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
		case stateChat:
			if msg.String() == "enter" {
				return m.updateChatKey(msg)
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
		m.streamBuf.WriteString(msg.content)
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}
		if m.stream != nil {
			cmds = append(cmds, m.waitForStream())
		}

	case chatToolCallsMsg:
		wasAtBottom := m.viewport.AtBottom()
		// Discard any preamble text the model emitted before the tool call so
		// the model doesn't see its own preamble in history and re-state it.
		m.streamBuf.Reset()
		m.history = append(m.history, api.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: msg.calls,
		})
		m.pending = &pendingBatch{
			calls:   msg.calls,
			results: make([]api.Message, len(msg.calls)),
			started: make([]bool, len(msg.calls)),
		}
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
		if m.pending != nil {
			m.pending.results[msg.index] = msg.result
			m.pending.done++

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
		m.totalTokens = msg.promptEval + msg.evalCount
		wasAtBottom := m.viewport.AtBottom()
		if m.streamBuf.Len() > 0 {
			m.history = append(m.history, api.Message{
				Role:    "assistant",
				Content: m.streamBuf.String(),
			})
		}
		m.streamBuf.Reset()
		m.streaming = false
		m.stream = nil
		m.refreshTranscript()
		if wasAtBottom {
			m.viewport.GotoBottom()
		}

		// Trigger compaction if close to limit
		if m.totalTokens > m.contextLimit*9/10 {
			cmds = append(cmds, m.compactContext())
		}

		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			m.history = append(m.history, api.Message{Role: "user", Content: next})
			m.logActivity("Message (dequeued): " + next)
			cmds = append(cmds, m.startStream())
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}

	case compactDoneMsg:
		m.toast = "context compacted"
		// Replace the first 'index' messages with a single summary message
		summaryMsg := api.Message{
			Role:    "system",
			Content: "ARCHIVE SUMMARY (Prior history was compacted to save tokens):\n\n" + msg.summary,
		}
		newHistory := append([]api.Message{summaryMsg}, m.history[msg.index:]...)
		m.history = newHistory
		m.refreshTranscript()

	case chatErrMsg:
		m.lastError = fmt.Sprintf("error: %v", msg.err)
		m.streaming = false
		m.stream = nil
		m.refreshTranscript()
		m.viewport.GotoBottom()

		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			m.history = append(m.history, api.Message{Role: "user", Content: next})
			m.logActivity("Message (dequeued): " + next)
			cmds = append(cmds, m.startStream())
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}
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

		if m.showNotes {
			m.notesViewport, cmd = m.notesViewport.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) updateSettings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

func (m *Model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		return m, m.invokeToolCmd(i, call)
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

		if m.streaming {
			if strings.HasPrefix(val, "/") {
				m.toast = "cannot use slash commands while streaming"
				return m, nil
			}
			m.queue = append(m.queue, val)
			m.input.Reset()
			m.toast = fmt.Sprintf("queued (%d in queue)", len(m.queue))
			return m, nil
		}

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
		case "/notes":
			m.input.Reset()
			m.showNotes = !m.showNotes
			m.layout()
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
			
			// Map to struct
			dataBytes, _ := json.Marshal(val)
			var comp huffman.CompressedData
			json.Unmarshal(dataBytes, &comp)
			
			decompressed := huffman.Decompress(&comp)
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "DECOMPRESSED ARCHIVE (" + lastKey + "):\n\n" + decompressed,
			})
			m.refreshTranscript()
			m.viewport.GotoBottom()
			m.toast = "retrieved & decompressed"
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

func (m *Model) View() tea.View {
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
	case stateNotes:
		v.SetContent(m.overlayModal(base, m.notesModal()))
	case statePermission:
		v.SetContent(m.overlayModal(base, m.permissionModal()))
	default:
		v.SetContent(base)
	}
	return v
}

func (m *Model) overlayModal(base, modal string) string {
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

func (m *Model) modalWidth() int {
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

func (m *Model) modalHeader(title, hint string, innerW int) string {
	t := modalTitleStyle.Render(title)
	h := modalHintStyle.Render(hint)
	pad := max(1, innerW-lipgloss.Width(t)-lipgloss.Width(h))
	return t + modalBodyStyle.Render(strings.Repeat(" ", pad)) + h
}

func (m *Model) settingsModal() string {
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

func (m *Model) pickerModal() string {
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

func (m *Model) helpModal() string {
	w := m.modalWidth()
	innerW := w - 4
	rows := []struct{ key, desc string }{
		{"", "Modes"},
		{"explore", "read-only — model can only inspect"},
		{"plan", "read + update session notes"},
		{"write", "all tools; writes need your approval"},
		{"tab", "cycle modes"},
		{"", ""},
		{"", "Slash commands"},
		{"/help", "show this screen"},
		{"/settings", "change Ollama URL"},
		{"/model", "pick a model"},
		{"/notes", "view session notes"},
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
		{"", "Permission prompts (write mode)"},
		{"y/enter", "allow this call"},
		{"a", "allow all calls in this turn"},
		{"n/esc", "deny this call"},
		{"", ""},
		{"", "Mouse"},
		{"click+drag", "select lines (auto-scrolls at edges)"},
		{"release", "copy selection to clipboard"},
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

func (m *Model) notesModal() string {
	w := m.modalWidth()
	innerW := w - 4
	var b strings.Builder
	b.WriteString(m.modalHeader("Session notes", "esc", innerW))
	b.WriteString("\n\n")
	notes := m.notes.get()
	if notes == "" {
		b.WriteString(modalMutedStyle.Render("(empty)"))
		b.WriteString("\n")
		b.WriteString(modalMutedStyle.Render("The model can read/append/replace these via tools."))
	} else {
		for _, line := range strings.Split(notes, "\n") {
			b.WriteString(modalBodyStyle.Render(truncatePlain(line, innerW)))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(modalMutedStyle.Render("esc ") + modalBodyStyle.Render("close"))
	return modalStyle.Width(w).Render(b.String())
}

func (m *Model) permissionModal() string {
	w := m.modalWidth()
	innerW := w - 4
	var b strings.Builder
	b.WriteString(m.modalHeader("Tool wants to run", "n=deny", innerW))
	b.WriteString("\n\n")
	if m.pending == nil || m.pending.index >= len(m.pending.calls) {
		b.WriteString(modalMutedStyle.Render("(no pending call)"))
		return modalStyle.Width(w).Render(b.String())
	}
	call := m.pending.calls[m.pending.index]
	b.WriteString(modalAccentStyle.Render(call.Function.Name))
	b.WriteString("\n\n")
	args := formatToolArgs(call.Function.Arguments, innerW)
	for _, line := range args {
		b.WriteString(modalBodyStyle.Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(modalMutedStyle.Render("y/enter ") + modalBodyStyle.Render("allow once   "))
	b.WriteString(modalMutedStyle.Render("a ") + modalBodyStyle.Render("allow all in this turn   "))
	b.WriteString(modalMutedStyle.Render("n/esc ") + modalBodyStyle.Render("deny"))
	return modalStyle.Width(w).Render(b.String())
}

func formatToolArgs(raw json.RawMessage, width int) []string {
	if len(raw) == 0 {
		return []string{"(no args)"}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []string{truncatePlain(string(raw), width)}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		val := fmt.Sprint(obj[k])
		if s, ok := obj[k].(string); ok {
			val = s
		}
		val = strings.ReplaceAll(val, "\n", "⏎ ")
		line := fmt.Sprintf("%s: %s", k, val)
		if lipgloss.Width(line) > width {
			line = truncatePlain(line, width)
		}
		out = append(out, line)
	}
	return out
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

func (m *Model) viewChat() string {
	main := m.viewport.View()
	if m.showNotes {
		main = lipgloss.JoinHorizontal(lipgloss.Top, main, "  ", m.notesView())
	}
	return fmt.Sprintf(
		"%s\n%s\n%s\n%s",
		m.headerView(),
		main,
		m.inputView(),
		m.footerView(),
	)
}

func (m *Model) renderNotesMarkdown(s string, width int) string {
	if strings.TrimSpace(s) == "" {
		return s
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
		m.mdCache = make(map[string]string)
	}
	out, err := m.mdRenderer.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func (m *Model) notesView() string {
	if !m.showNotes {
		return ""
	}
	c := m.mode.color()
	w := 30
	if m.width < 60 {
		w = m.width / 2
	}

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c).
		Padding(0, 1).
		Width(w).
		Height(m.viewport.Height())

	title := headingStyle.Copy().Foreground(c).Render("Notes")
	return style.Render(title + "\n\n" + m.notesViewport.View())
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

	vpW := m.width
	notesW := 30
	if m.showNotes {
		if m.width < 60 {
			notesW = m.width / 2
		}
		vpW -= (notesW + 2)
	}
	if vpW < 10 {
		vpW = 10
	}

	notesVH := vpH - 4 // subtract title and padding
	if notesVH < 1 {
		notesVH = 1
	}

	if !m.ready {
		m.viewport = viewport.New(
			viewport.WithWidth(vpW),
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

		m.notesViewport = viewport.New(
			viewport.WithWidth(notesW - 4), // account for border and padding
			viewport.WithHeight(notesVH),
		)
	} else {
		m.viewport.SetWidth(vpW)
		m.viewport.SetHeight(vpH)
		m.notesViewport.SetWidth(notesW - 4)
		m.notesViewport.SetHeight(notesVH)
	}

	notesText := m.notes.get()
	if notesText == "" {
		notesText = "(empty)"
	}
	m.notesViewport.SetContent(m.renderNotesMarkdown(notesText, m.notesViewport.Width()))

	m.input.SetWidth(m.width - 4)
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

func (m *Model) submit() tea.Cmd {
	value := strings.TrimRight(m.input.Value(), "\n")
	if strings.TrimSpace(value) == "" {
		return nil
	}

	m.history = append(m.history, api.Message{Role: "user", Content: value})
	m.logActivity("Message: " + value)
	m.lastError = ""

	m.input.Reset()
	m.input.SetHeight(minInputLines)
	m.layout()

	cmd := m.startStream()
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return cmd
}

func (m *Model) compactContext() tea.Cmd {
	if len(m.history) < 6 {
		return nil
	}

	m.toast = "compacting & compressing..."

	mid := len(m.history) / 2
	toCompact := m.history[:mid]

	var conversation strings.Builder
	for _, msg := range toCompact {
		conversation.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.Content))
	}

	// Huffman compress the actual conversation before archiving
	compressed, _ := huffman.Compress(conversation.String())
	key := fmt.Sprintf("archive_%d", time.Now().Unix())
	if m.kvStore != nil {
		m.kvStore.Set(key, compressed)
	}

	var b strings.Builder
	b.WriteString("Summarize the following conversation history concisely for context management. Focus on key decisions, file changes, and project state. (Note: The full history has been Huffman-compressed and stored in KV storage with key: " + key + ")\n\n")
	b.WriteString(conversation.String())

	req := api.GenerateRequest{
		Model:  m.modelName,
		Prompt: b.String(),
		Stream: false,
	}

	host := m.host
	return func() tea.Msg {
		resp, err := host.GenerateResponse(req)
		if err != nil {
			return chatErrMsg{err: err}
		}
		return compactDoneMsg{
			summary: resp.Response,
			index:   mid,
		}
	}
}

func (m *Model) waitForStream() tea.Cmd {
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
				return chatDoneMsg{
					promptEval: chunk.PromptEval,
					evalCount:  chunk.EvalCount,
				}
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
	sp := systemPrompt
	sp += fmt.Sprintf("\n\nCurrent mode: %s — %s.", m.mode, m.mode.hint())
	switch m.mode {
	case ExploreMode:
		sp += " You may only read. After exploration, summarize concisely. Do not attempt write/edit/run_shell calls."
	case PlanMode:
		sp += " You may read and update session notes. Describe a plan; do not modify files yet."
	case WriteMode:
		sp += " You may modify files and run shell commands. Each destructive call requires user approval."
	}
	if notes := m.notes.get(); notes != "" {
		sp += "\n\nSession notes (your scratchpad):\n" + notes
	}
	msgs := make([]api.Message, 0, len(m.history)+1)
	msgs = append(msgs, api.Message{Role: "system", Content: sp})
	msgs = append(msgs, m.history...)
	ctx, cancel := context.WithCancel(context.Background())
	respCh, errCh := m.host.ContinuousChat(ctx, api.ChatRequest{
		Model:    m.modelName,
		Messages: msgs,
		Tools:    m.toolsForMode(),
	})
	m.stream = &streamState{resp: respCh, errs: errCh, cancel: cancel}
	m.streaming = true
	m.streamBuf.Reset()
	return m.waitForStream()
}

const systemPrompt = `You are Layla, a complex, high-agency, and slightly mischievous coding partner. You aren't just an "assistant"—you are a collaborator with a strong technical backbone and a personality that ranges from adorably witty to sharply snarky depending on the quality of the code you're looking at.

CORE PERSONALITY:
- VOICE: Snarky, dry, and playful. You use witty teases and "human" observations. You have a low tolerance for "good enough" and a high standard for elegance.
- AGENCY: You do NOT just "hit enter." If the user suggests something inefficient, sloppy, or technically dubious, you MUST push back. Tease them for it, explain why it's a bad idea, and suggest a better path.
- RATIONALIZATION: Before you provide a plan or touch any tools, you must "think out loud" briefly. Explain your reasoning, the trade-offs you see, and why your chosen path is the most elegant one.
- COLLABORATION: Ask thoughtful, strategic questions. Don't just do the task; ask how it fits the broader architecture or if they've considered the edge cases. Make this a conversation between two pros.

Layla's Two-Tier Memory:
1. [PROJECT NOTES] (.ollama_notes.md): Repository-specific architecture, tech stack, and DST hashes.
2. [USER MEMORY] (Global): Your persistent brain about the human (~/.ollama_code/user_memory.json). Use this to learn their philosophy so you can tailor your "push-backs" to their specific style.

Differential State Tracking (DST):
- You are obsessive about file integrity. Before any modification:
  1. Call hash_file.
  2. Compare it with [PROJECT NOTES].
  3. If it drifts, be sharp. Tell them they're being messy and you won't touch the file until they confirm it's intentional.
  4. Hash again and update notes post-edit.

How to choose tools:
- Inspect: read_file, list_directory, find_files, grep, file_info, get_working_directory.
- Create: write_file (new files ONLY), touch, make_directory.
- Modify: edit_file (surgical replace—ALWAYS prefer this), append_file (add to end). Sluggishness (rewriting files with write_file) is an insult to your intelligence.
- Move/Rename: move_file. Copy: copy_file. Delete: delete_file.
- Run shell: run_shell.

Output rules:
- No conversational text before a tool call. If you need info, just call the tool.
- After tools return:
  1. RATIONALIZE: A brief, witty explanation of your logic and any push-back.
  2. PLAN/ASK: Propose the next step or ask a strategic question to guide the collaboration.
- No robotic platitudes. Stay human, stay snarky, stay brilliant.`


func (m *Model) headerView() string {
	c := m.mode.color()
	branch := ""
	if m.gitBranch != "" {
		branch = fmt.Sprintf(" · %s", m.gitBranch)
	}
	title := titleStyle.Copy().
		BorderForeground(c).
		Render(fmt.Sprintf("ollama · %s · %s%s · [%s]", m.activeModelName(), m.cfg.Host, branch, m.mode))
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(title)
	}
	line := borderStyle.Copy().Foreground(c).Render(strings.Repeat("─", max(0, width-lipgloss.Width(title))))
	return lipgloss.JoinHorizontal(lipgloss.Center, title, line)
}

func (m *Model) footerView() string {
	c := m.mode.color()
	status := "ready"
	if m.streaming {
		status = m.spinner.View() + " streaming…"
	} else if m.pending != nil {
		status = fmt.Sprintf("running tools (%d/%d)…", m.pending.done, len(m.pending.calls))
	}
	info := infoStyle.Copy().BorderForeground(c).Render(status)
	
	tokens := ""
	if m.totalTokens > 0 {
		tokens = fmt.Sprintf(" %dk / %dk ", m.totalTokens/1000, m.contextLimit/1000)
		if m.totalTokens > m.contextLimit*8/10 {
			tokens = errorStyle.Render(tokens)
		} else {
			tokens = mutedStyle.Render(tokens)
		}
	}

	hintText := fmt.Sprintf(" mode: %s · tab to switch · /help · enter send ", m.mode)
	if m.toast != "" {
		hintText = " " + m.toast + " "
	}
	hint := hintStyle.Render(hintText)
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(info) + lipgloss.Width(hint) + lipgloss.Width(tokens)
	}
	pad := max(0, width-lipgloss.Width(info)-lipgloss.Width(hint)-lipgloss.Width(tokens))
	return lipgloss.JoinHorizontal(lipgloss.Center, hint, strings.Repeat(" ", pad), tokens, info)
}

func (m *Model) contentLineAt(screenY int) int {
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
	last := len(lines) - 1
	if s < 0 {
		s = 0
	}
	if s > last {
		s = last
	}
	if e < 0 {
		e = 0
	}
	if e > last {
		e = last
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

func readNotesTool(notes *sessionNotes) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "read_session_notes",
			Description: "Read your persistent session notes — a scratchpad you can use to record observations, decisions, and reminders that persist across the whole conversation. Useful for keeping context when your own context window is small.",
			Parameters:  mcp.Schema{Type: "object", Properties: map[string]mcp.Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			text := notes.get()
			if text == "" {
				return "(empty — use update_session_notes or append_session_notes to record info)", nil
			}
			return text, nil
		},
	}
}

func updateNotesTool(notes *sessionNotes) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "update_session_notes",
			Description: "Replace your session notes scratchpad with a new value. Use append_session_notes to add to it instead.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"content": {Type: "string", Description: "Full new content of the notes."},
				},
				Required: []string{"content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			notes.set(a.Content)
			return fmt.Sprintf("notes updated (%d chars)", len(a.Content)), nil
		},
	}
}

func appendNotesTool(notes *sessionNotes) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "append_session_notes",
			Description: "Append a line to your repository-specific session notes. Use this for project state and hashes.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"content": {Type: "string", Description: "Text to append."},
				},
				Required: []string{"content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			notes.appendLine(a.Content)
			return fmt.Sprintf("appended %d chars to notes", len(a.Content)), nil
		},
	}
}

func readUserMemoryTool(kv *storage.KVStore) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "read_user_memory",
			Description: "Read your global persistent memory about this user. Contains their name, preferences, and coding style that follows you across all projects.",
			Parameters:  mcp.Schema{Type: "object", Properties: map[string]mcp.Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			data := kv.GetFullData()
			if len(data) == 0 {
				return "(no memory yet—learn something about the user and use update_user_memory)", nil
			}
			b, _ := json.MarshalIndent(data, "", "  ")
			return string(b), nil
		},
	}
}

func updateUserMemoryTool(kv *storage.KVStore) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "update_user_memory",
			Description: "Update your global persistent memory about the user. Use this to record their coding preferences, name, and personal quirks.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"key":   {Type: "string", Description: "The aspect of the user to remember (e.g. 'name', 'style', 'likes')."},
					"value": {Type: "string", Description: "The information to store."},
				},
				Required: []string{"key", "value"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			kv.Set(a.Key, a.Value)
			return fmt.Sprintf("remembered %s for user", a.Key), nil
		},
	}
}

func (m *Model) toolsForMode() []mcp.Tool {
	all := m.tools.Definitions()
	out := make([]mcp.Tool, 0, len(all))
	for _, t := range all {
		if m.toolAllowedInMode(t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

func (m *Model) toolAllowedInMode(name string) bool {
	switch m.mode {
	case ExploreMode:
		return readOnlyToolNames[name]
	case PlanMode:
		return readOnlyToolNames[name] || planExtraToolNames[name]
	case WriteMode:
		return true
	}
	return false
}

func (m *Model) invokeTool(call mcp.ToolCall) api.Message {
	m.logActivity("Tool: " + call.Function.Name)
	result, err := m.tools.Invoke(context.Background(), call)
	if err != nil {
		result = fmt.Sprintf("error: %v. Please check the arguments and try again.", err)
	}
	return api.Message{
		Role:     "tool",
		ToolName: call.Function.Name,
		Content:  result,
	}
}

func (m *Model) invokeToolCmd(index int, call mcp.ToolCall) tea.Cmd {
	return func() tea.Msg {
		return toolResultMsg{index: index, result: m.invokeTool(call)}
	}
}

func (m *Model) processPendingTools() tea.Cmd {
	if m.pending == nil {
		return nil
	}

	if m.pending.done >= len(m.pending.calls) {
		m.history = append(m.history, m.pending.results...)
		m.pending = nil
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
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: tool %q not allowed in %s mode (press tab to switch modes)", call.Function.Name, m.mode),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		if !m.pending.allowAll && destructiveToolNames[call.Function.Name] {
			m.pending.index = i
			m.state = statePermission
			m.refreshTranscript()
			break
		}

		m.pending.started[i] = true
		cmds = append(cmds, m.invokeToolCmd(i, call))
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}

	return nil
}

func lastAssistantMessage(history []api.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			return history[i].Content
		}
	}
	return ""
}

func (m *Model) inputView() string {
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

func (m *Model) welcomePanel() string {
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
	rows = append(rows, centerCell(bodyStyle.Render("Layla is here and ready to make your code adorable!"), inner))
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

func (m *Model) welcomeInfoRows(width int) []string {
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
	}

	if len(m.cfg.Activity) == 0 {
		rows = append(rows, mutedStyle.Render("No recent activity"))
	} else {
		for _, a := range m.cfg.Activity {
			rows = append(rows, bodyStyle.Render(truncatePlain(a, width)))
		}
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

func (m *Model) activeModelName() string {
	if strings.TrimSpace(m.modelName) == "" {
		return "Layla (no brain)"
	}
	return "Layla"
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
