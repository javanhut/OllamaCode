package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	glamourAnsi "github.com/charmbracelet/glamour/ansi"
	glamourStyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/companion"
	"github.com/javanhut/ollama_code/internal/huffman"
	"github.com/javanhut/ollama_code/internal/memory"
	"github.com/javanhut/ollama_code/internal/semantic"
	"github.com/javanhut/ollama_code/internal/session"
	"github.com/javanhut/ollama_code/internal/storage"
	"github.com/javanhut/ollama_code/mcp"
)

// invisibleTools are tools whose calls and results are hidden from the
// transcript. The model still receives the result; the user just sees the
// natural-language acknowledgement.
var invisibleTools = map[string]bool{
	"remember": true,
	"recall":   true,
	"forget":   true,
}

const DefaultHost = "http://localhost:11434"

var (
	accentColor    = lipgloss.Color("211")
	secondaryColor = lipgloss.Color("81")
	textColor      = lipgloss.Color("252")
	surfaceColor   = lipgloss.Color("236")
	panelColor     = lipgloss.Color("237")
	subtleColor    = lipgloss.Color("240")

	borderStyle    = lipgloss.NewStyle().Foreground(subtleColor)
	inputBandStyle = lipgloss.NewStyle().Background(panelColor).Foreground(textColor)
	chromeStyle    = lipgloss.NewStyle().Background(surfaceColor).Foreground(textColor)
	userStyle      = lipgloss.NewStyle().Foreground(secondaryColor).Bold(true)
	assistantStyle = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	hintStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	headingStyle   = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	asciiStyle     = lipgloss.NewStyle().Foreground(secondaryColor).Bold(true)
	bodyStyle      = lipgloss.NewStyle().Foreground(textColor)
	selectionStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Foreground(lipgloss.Color("230"))

	modalBg = surfaceColor

	modalStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(subtleColor).
			Background(modalBg).
			Foreground(textColor).
			Padding(1, 2)

	modalTitleStyle  = lipgloss.NewStyle().Foreground(textColor).Background(modalBg).Bold(true)
	modalHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(modalBg)
	modalBodyStyle   = lipgloss.NewStyle().Foreground(textColor).Background(modalBg)
	modalMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(modalBg)
	modalAccentStyle = lipgloss.NewStyle().Foreground(accentColor).Background(modalBg).Bold(true)
	modalErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Background(modalBg).Bold(true)
	modalSelectStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Background(secondaryColor).Bold(true)
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

func parseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "explore":
		return ExploreMode, true
	case "plan":
		return PlanMode, true
	case "write":
		return WriteMode, true
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
// through isExploreReadOnlyShell before invocation.
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
}

type config struct {
	Host     string   `json:"host"`
	Model    string   `json:"model,omitempty"`
	Activity []string `json:"activity,omitempty"`
	Verbose  bool     `json:"verbose,omitempty"`

	MaxSteps   int                     `json:"max_steps,omitempty"`   // tool-call budget per user turn (default 25)
	EmbedModel string                  `json:"embed_model,omitempty"` // model for auto-RAG embeddings
	AutoRAG    *bool                   `json:"auto_rag,omitempty"`    // nil/true = enabled
	Dream      *bool                   `json:"dream,omitempty"`       // nil/true = dream mode enabled
	Verify     *bool                   `json:"verify,omitempty"`      // nil/true = auto compile-check on file edits
	VerifyCmd  string                  `json:"verify_cmd,omitempty"`  // override the auto-detected check
	Profiles   map[string]ModelProfile `json:"profiles,omitempty"`    // per-model, keyed by model name
}

// ModelProfile holds per-model settings discovered from /api/show (and cached)
// plus optional sampling overrides, so num_ctx and tool support adapt to the
// actual model instead of a hardcoded value.
type ModelProfile struct {
	NumCtx        int      `json:"num_ctx"`
	SupportsTools bool     `json:"supports_tools"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	NumPredict    *int     `json:"num_predict,omitempty"`
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
	content    string
	promptEval int
	evalCount  int
}
type chatErrMsg struct{ err error }
type chatToolCallsMsg struct {
	content string
	calls   []mcp.ToolCall
}

type toolResultMsg struct {
	index      int
	result     api.Message
	modeSwitch *modeSwitchRequest
}

type modeSwitchRequest struct {
	target Mode
	mode   string
	reason string
}

type compactDoneMsg struct {
	summary string
	index   int
}

type modelsLoadedMsg struct {
	models []string
}
type connectErrMsg struct{ err error }

type companionTranscriptMsg struct{ text string }
type companionErrorMsg struct{ err error }
type companionStoppedMsg struct{}

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
	preview  string
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
	profile   ModelProfile
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

	totalTokens    int
	contextLimit   int
	archiveSummary string // rolling summary of compacted-away history (volatile tail)
	compacting     bool   // guards against overlapping compaction passes

	// Loop safety (reset each user turn).
	stepCount          int            // tool-call rounds since the last user message
	maxSteps           int            // budget per turn (cfg.MaxSteps, default 25)
	recentCalls        []string       // ring of recent call fingerprints (oscillation)
	failedCalls        map[string]int // fingerprint -> consecutive failure count
	oscillationWarned  bool           // corrective nudge emitted once per turn
	suppressToolsOnce  bool           // next stream sends no tools (step budget hit)
	lastStepTool       string         // tool name of the previous step's batch
	sameToolStreak     int            // consecutive steps calling only that tool
	turnTouchedFiles   bool           // a file-mutating tool succeeded this turn
	verifying          bool           // a compile check is running
	verifyAttempts     int            // failed compile checks this turn
	challengedThisTurn bool           // self-check challenge already issued this turn

	// Auto-RAG. Published indexes are treated immutable; background reindex
	// works on a Clone and delivers a replacement via ragRefreshedMsg.
	ragIndex     *semantic.Index
	ragReady     bool
	ragBuilding  bool
	lastRagQuery string
	lastRagBlock string // injected via buildDynamicContext; reused across tool re-invokes
	ragMu        sync.Mutex
	ragChanged   map[string]bool // paths changed since last reindex (hook-populated)

	ckpt checkpointStore // per-turn file snapshots for /undo

	// Dream mode: idle-triggered background reflection.
	lastActivity     time.Time
	asleep           bool
	dreaming         bool
	dreamCount       int
	lastDreamAt      time.Time
	dreams           []dream // full session log (/dreams)
	pendingDreams    []dream // dreams since last wake, surfaced on return
	dreamCancel      context.CancelFunc
	notesBackup      string // pre-consolidation notes, restorable via /notes restore
	kvStore          *storage.KVStore
	memory           *memory.Store
	mdCache          map[string]string
	expandTools      bool
	slashVisible     bool
	slashSuggestions []string
	slashSelected    int

	userHistory     []string
	historyIndex    int
	companion       *companion.Client
	companionSender func(tea.Msg)
	lastRenderTime  time.Time
	busySince       time.Time
}

var slashCommands = []struct {
	name string
	desc string
}{
	{"/quit", "exit the application"},
	{"/exit", "exit the application"},
	{"/settings", "change Ollama URL"},
	{"/model", "pick a model"},
	{"/models", "pick a model"},
	{"/clear", "reset the conversation"},
	{"/help", "show help screen"},
	{"/?", "show help screen"},
	{"/notes", "toggle session notes panel"},
	{"/companion", "toggle speech-to-text input"},
	{"/copy", "copy last response to clipboard"},
	{"/save", "save session with optional name"},
	{"/load", "load a saved session by name"},
	{"/sessions", "list saved sessions"},
	{"/archive", "retrieve compressed archive"},
	{"/undo", "revert the last turn's file changes"},
	{"/clearnotes", "clear the session notes scratchpad"},
	{"/dreams", "show what it dreamt about while idle"},
	{"/dream", "toggle idle dream mode on/off"},
	{"/verify", "toggle auto compile-check after edits"},
	{"/verbose", "toggle detailed tool output"},
}

func (m *Model) updateSlashSuggestions() {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") || strings.Contains(val, " ") || strings.Contains(val, "\n") {
		m.slashVisible = false
		m.slashSuggestions = nil
		m.slashSelected = 0
		return
	}
	var matches []string
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, val) && val != c.name {
			dup := false
			for _, m := range matches {
				if m == c.name {
					dup = true
					break
				}
			}
			if !dup {
				matches = append(matches, c.name)
			}
		}
	}
	if len(matches) > 0 {
		m.slashVisible = true
		m.slashSuggestions = matches
		if m.slashSelected >= len(matches) {
			m.slashSelected = 0
		}
	} else {
		m.slashVisible = false
		m.slashSuggestions = nil
		m.slashSelected = 0
	}
}

func New() *Model {
	cfg := loadConfig()
	// ... (host setup ...)
	host := api.OllamaHost{}
	host.SetURI(cfg.Host)

	archivePath := filepath.Join(os.Getenv("HOME"), ".ollama_code", "archive.json")
	memoryPath := filepath.Join(os.Getenv("HOME"), ".ollama_code", "user_memory.json")

	kv, _ := storage.NewKVStore(archivePath)
	mem, _ := memory.New(memoryPath)

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
	ta.DynamicHeight = true
	ta.MinHeight = minInputLines
	ta.MaxHeight = maxInputLines
	ta.SetHeight(minInputLines)
	styles := ta.Styles()
	inputBg := panelColor
	styles.Focused.Base = lipgloss.NewStyle().Background(inputBg).Padding(0, 1)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(secondaryColor).Background(inputBg).Bold(true)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(inputBg)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor).Background(inputBg)
	styles.Focused.CursorLine = lipgloss.NewStyle().Background(inputBg)
	styles.Blurred.Base = lipgloss.NewStyle().Background(inputBg).Padding(0, 1)
	styles.Blurred.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(inputBg)
	styles.Blurred.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Background(inputBg)
	styles.Blurred.Text = lipgloss.NewStyle().Foreground(textColor).Background(inputBg)
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
	registry.Register(rememberTool(mem))
	registry.Register(recallTool(mem))
	registry.Register(forgetTool(mem))
	registry.Register(mcp.CodeIndexTool(host))
	registry.Register(mcp.SemanticSearchTool(host))

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(accentColor)

	m := &Model{
		cfg:          cfg,
		host:         host,
		tools:        registry,
		notes:        notes,
		mode:         ExploreMode,
		state:        stateChat,
		urlInput:     ti,
		input:        ta,
		modelName:    cfg.Model,
		spinner:      s,
		gitBranch:    getGitBranch(),
		transcript:   &strings.Builder{},
		streamBuf:    &strings.Builder{},
		contextLimit: defaultContextLimit,
		profile:      ModelProfile{NumCtx: defaultContextLimit, SupportsTools: true},
		maxSteps:     maxStepsFromConfig(cfg),
		failedCalls:  make(map[string]int),
		kvStore:      kv,
		memory:       mem,
		mdCache:      make(map[string]string),
	}

	m.lastActivity = time.Now()
	registry.Register(m.switchModeTool())
	registry.Register(m.spawnSubagentTool())
	registry.SetFileChangeHook(m.noteFileChanged)
	if m.modelName != "" {
		m.resolveProfile()
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
		// Group consecutive assistant + tool messages into a single Layla
		// turn so the user sees one block per response, not one per tool call.
		i := 0
		var openTurn *assistantTurn
		flushTurn := func() {
			if openTurn != nil {
				m.writeAssistantTurn(&b, openTurn, false)
				openTurn = nil
			}
		}
		for i < len(m.history) {
			msg := m.history[i]
			switch msg.Role {
			case "user":
				flushTurn()
				b.WriteString(userStyle.Render("You"))
				b.WriteString("\n")
				b.WriteString(msg.Content)
				b.WriteString("\n\n")
				i++
			case "assistant", "tool":
				turn, next := m.collectAssistantTurn(i)
				openTurn = &turn
				i = next
			default:
				flushTurn()
				if msg.Content != "" {
					b.WriteString(m.renderMarkdown(msg.Content, true))
					b.WriteString("\n\n")
				}
				i++
			}
		}

		if m.streaming {
			if openTurn == nil {
				openTurn = &assistantTurn{}
			}
			openTurn.streaming = true
			if m.streamBuf.Len() > 0 {
				openTurn.contents = append(openTurn.contents, m.streamBuf.String())
			}
		}
		flushTurn()
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

// assistantTurn is one rendered Layla block: all assistant content between
// two user messages, plus the visible tool calls fired during it.
type assistantTurn struct {
	contents  []string
	toolCalls []toolCallEntry
	streaming bool
}

type toolCallEntry struct {
	call      mcp.ToolCall
	result    string
	hasResult bool
}

// collectAssistantTurn walks history starting at i, gathering every
// contiguous assistant/tool message into a single turn. Returns the turn and
// the index of the next non-assistant/non-tool message (or len(history)).
func (m *Model) collectAssistantTurn(start int) (assistantTurn, int) {
	var t assistantTurn
	consumed := make(map[int]bool)
	i := start
	for i < len(m.history) {
		msg := m.history[i]
		if msg.Role != "assistant" && msg.Role != "tool" {
			break
		}
		if msg.Role == "assistant" {
			if msg.Content != "" {
				t.contents = append(t.contents, msg.Content)
			}
			for _, call := range msg.ToolCalls {
				resultIdx := -1
				for j := i + 1; j < len(m.history); j++ {
					if m.history[j].Role != "tool" && m.history[j].Role != "assistant" {
						break
					}
					if !consumed[j] && m.history[j].Role == "tool" && m.history[j].ToolName == call.Function.Name {
						resultIdx = j
						consumed[j] = true
						break
					}
				}
				if invisibleTools[call.Function.Name] {
					continue
				}
				entry := toolCallEntry{call: call}
				if resultIdx >= 0 {
					entry.result = m.history[resultIdx].Content
					entry.hasResult = true
				}
				t.toolCalls = append(t.toolCalls, entry)
			}
		}
		i++
	}
	return t, i
}

// writeAssistantTurn renders a turn as a single Layla block: header,
// concatenated content, then tool calls — collapsed by default, expanded when
// the user has toggled `ctrl+t`.
func (m *Model) writeAssistantTurn(b *strings.Builder, t *assistantTurn, _ bool) {
	b.WriteString(assistantStyle.Copy().Foreground(m.mode.color()).Render(m.activeModelName()))
	b.WriteString("\n")

	fullContent := strings.Join(t.contents, "")
	if fullContent != "" {
		b.WriteString(m.renderMarkdown(fullContent, true))
		b.WriteString("\n")
	}

	if t.streaming && fullContent == "" && m.pending == nil {
		b.WriteString(m.spinner.View())
		b.WriteString(mutedStyle.Render(" Thinking..."))
		b.WriteString("\n")
	}

	if len(t.toolCalls) > 0 {
		b.WriteString("\n")
		if m.expandTools {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("▾ %d tool call%s (ctrl+t to collapse)",
				len(t.toolCalls), plural(len(t.toolCalls)))))
			b.WriteString("\n")
			for _, entry := range t.toolCalls {
				if entry.hasResult {
					b.WriteString(m.renderMarkdown(renderCollapsedTool(entry.call, entry.result, m.cfg.Verbose), true))
				} else {
					b.WriteString(m.renderMarkdown(renderToolCall(entry.call, m.cfg.Verbose), true))
				}
				b.WriteString("\n")
			}
		} else {
			names := make([]string, 0, len(t.toolCalls))
			for _, entry := range t.toolCalls {
				names = append(names, entry.call.Function.Name)
			}
			summary := fmt.Sprintf("▸ %d tool call%s — %s · ctrl+t to expand",
				len(t.toolCalls), plural(len(t.toolCalls)), strings.Join(uniqueNames(names), ", "))
			b.WriteString(mutedStyle.Render(summary))
			b.WriteString("\n")
		}
	}

	if t.streaming && m.pending != nil && m.pending.done < len(m.pending.calls) {
		b.WriteString(m.spinner.View())
		label := m.currentToolLabel()
		if label != "" {
			b.WriteString(mutedStyle.Render(fmt.Sprintf(" running %s… (%d/%d)", label, m.pending.done, len(m.pending.calls))))
		} else {
			b.WriteString(mutedStyle.Render(" working…"))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func uniqueNames(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
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

// laylaMarkdownStyle returns a glamour style based on the dark theme but with
// heading prefixes ("##", "###", …) stripped and replaced with bold colored
// titles, so headings actually look like headings instead of literal hashes.
func laylaMarkdownStyle() glamourAnsi.StyleConfig {
	s := glamourStyles.DarkStyleConfig
	bold := true
	makeHeading := func(color string, prefix string) glamourAnsi.StyleBlock {
		return glamourAnsi.StyleBlock{
			StylePrimitive: glamourAnsi.StylePrimitive{
				BlockPrefix: "\n",
				BlockSuffix: "\n",
				Prefix:      prefix,
				Color:       &color,
				Bold:        &bold,
			},
		}
	}
	s.H2 = makeHeading("39", "")
	s.H3 = makeHeading("45", "")
	s.H4 = makeHeading("51", "")
	s.H5 = makeHeading("80", "")
	s.H6 = makeHeading("110", "")
	return s
}

// LaTeX math notation patterns. We rewrite $…$ / $$…$$ as inline code so
// the user sees a styled span instead of literal dollar signs (glamour has no
// math renderer). Currency like "$5" doesn't match because it has no closer.
var (
	mathDisplayRe = regexp.MustCompile(`\$\$([^\n$]+?)\$\$`)
	mathInlineRe  = regexp.MustCompile(`\$([^\s$](?:[^$\n]*?[^\s$])?)\$`)
)

// stripLatexMath converts $…$ and $$…$$ into Markdown inline code, skipping
// content inside fenced code blocks where the dollars might be intentional.
func stripLatexMath(s string) string {
	if !strings.Contains(s, "$") && !strings.Contains(s, "\\(") && !strings.Contains(s, "\\[") {
		return s
	}

	// Handle multi-line $$ ... $$ and \[ ... \] blocks by converting to code blocks
	// Handle inline $ ... $ and \( ... \) by converting to inline code
	s = regexp.MustCompile(`(?s)\$\$(.*?)\$\$`).ReplaceAllString(s, "```latex\n$1\n```")
	s = regexp.MustCompile(`(?s)\\\[(.*?)\\\]`).ReplaceAllString(s, "```latex\n$1\n```")
	s = regexp.MustCompile(`\\\((.*?)\\\)`).ReplaceAllString(s, "`$1`")
	s = mathInlineRe.ReplaceAllString(s, "`$1`")

	return s
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

	pre := stripLatexMath(s)

	width := m.viewport.Width()
	if width <= 4 {
		width = 80
	}
	wrap := width - 2
	if m.mdRenderer == nil || m.mdWidth != wrap {
		r, err := glamour.NewTermRenderer(
			glamour.WithStyles(laylaMarkdownStyle()),
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
	out, err := m.mdRenderer.Render(pre)
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
		m.mdRenderer = nil
		m.mdCache = make(map[string]string)
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
		// Tab: complete slash command if autocomplete is visible, else cycle mode.
		if msg.String() == "tab" && (m.state == stateChat || m.state == stateHelp || m.state == stateNotes) {
			if m.slashVisible && len(m.slashSuggestions) > 0 {
				// Complete with current suggestion
				m.input.SetValue(m.slashSuggestions[m.slashSelected])
				m.input.CursorEnd()
				m.slashVisible = false
				m.slashSuggestions = nil
				m.slashSelected = 0
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
		case stateChat:
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
		wasAtBottom := m.viewport.AtBottom()
		preamble := m.streamBuf.String()
		m.streamBuf.Reset()
		m.history = append(m.history, api.Message{
			Role:      "assistant",
			Content:   preamble,
			ToolCalls: msg.calls,
		})
		m.pending = &pendingBatch{
			calls:   msg.calls,
			results: make([]api.Message, len(msg.calls)),
			started: make([]bool, len(msg.calls)),
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
		if m.pending != nil {
			m.pending.results[msg.index] = msg.result
			m.pending.done++
			if msg.index < len(m.pending.calls) {
				call := m.pending.calls[msg.index]
				if strings.HasPrefix(msg.result.Content, "error:") {
					m.failedCalls[callFingerprint(call)]++
				} else if len(mcp.MutatedPaths(call.Function.Name, call.Function.Arguments)) > 0 {
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
		if parsed := m.tools.ParseToolCallsFromContent(finalAssistant); len(parsed) > 0 && m.stepCount < m.maxSteps {
			m.history = append(m.history, api.Message{
				Role:      "assistant",
				Content:   finalAssistant,
				ToolCalls: parsed,
			})
			m.pending = &pendingBatch{
				calls:   parsed,
				results: make([]api.Message, len(parsed)),
				started: make([]bool, len(parsed)),
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
			}
			m.refreshTranscript()
			if m.companion != nil && finalAssistant != "" {
				_ = m.companion.Speak(finalAssistant)
			}
			if wasAtBottom {
				m.viewport.GotoBottom()
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
		idx := msg.index
		if idx > len(m.history) {
			idx = len(m.history)
		}
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
		m.lastError = fmt.Sprintf("error: %v", msg.err)
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
	case stateChat:
		prevH := m.input.Height()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		m.updateSlashSuggestions()
		if m.input.Height() != prevH {
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
			if m.streaming && m.stream != nil {
				m.stream.cancel()
			}
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
		{"/companion", "toggle GUI popup (speech in <-> input, replies -> TTS)"},
		{"/save", "save current conversation to named session"},
		{"/load", "load a saved session"},
		{"/sessions", "list saved sessions"},
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
		{"ctrl+t", "expand/collapse tool calls in transcript"},
		{"ctrl+s/esc", "stop a streaming response"},
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

	// We want the total lines of the modal content to fit within m.height - 4 (to account for borders & padding)
	maxLines := m.height - 4
	if maxLines < 8 {
		maxLines = 8 // absolute minimum safety boundary
	}

	var headerSection []string
	headerSection = append(headerSection, m.modalHeader("Tool wants to run", "n=deny", innerW))
	headerSection = append(headerSection, "")

	if m.pending == nil || m.pending.index >= len(m.pending.calls) {
		headerSection = append(headerSection, modalMutedStyle.Render("(no pending call)"))
		return modalStyle.Width(w).Render(strings.Join(headerSection, "\n"))
	}

	call := m.pending.calls[m.pending.index]
	headerSection = append(headerSection, modalAccentStyle.Render(call.Function.Name))
	headerSection = append(headerSection, "")

	var footerSection []string
	footerSection = append(footerSection, "")
	footerSection = append(footerSection, modalMutedStyle.Render("y/enter ")+modalBodyStyle.Render("allow once   ")+
		modalMutedStyle.Render("a ")+modalBodyStyle.Render("allow all in this turn   ")+
		modalMutedStyle.Render("n/esc ")+modalBodyStyle.Render("deny"))

	// How many lines are left for arguments and preview?
	usedLines := len(headerSection) + len(footerSection) + 2
	availableLines := maxLines - usedLines
	if availableLines < 2 {
		availableLines = 2 // absolute minimum for args/preview
	}

	var middleSection []string
	args := formatToolArgs(call.Function.Arguments, innerW)

	// Append arguments line-by-line, truncating if they exceed availableLines
	for i, line := range args {
		if len(middleSection) >= availableLines-1 && i < len(args)-1 {
			middleSection = append(middleSection, modalMutedStyle.Render(fmt.Sprintf("... (%d more lines of arguments)", len(args)-i)))
			break
		}
		middleSection = append(middleSection, modalBodyStyle.Render(line))
	}

	// If there's still room, add the preview
	remainingForPreview := availableLines - len(middleSection)
	if m.pending.preview != "" && remainingForPreview > 2 {
		middleSection = append(middleSection, "")
		middleSection = append(middleSection, modalMutedStyle.Render("Preview:"))
		remainingForPreview -= 2

		previewLines := strings.Split(m.pending.preview, "\n")
		for i, line := range previewLines {
			if len(middleSection) >= availableLines-1 && i < len(previewLines)-1 {
				middleSection = append(middleSection, modalMutedStyle.Render(fmt.Sprintf("... (%d more lines of preview)", len(previewLines)-i)))
				break
			}
			middleSection = append(middleSection, modalMutedStyle.Render(truncatePlain(line, innerW)))
		}
	}

	// Build the final modal string
	var b strings.Builder
	for _, line := range headerSection {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, line := range middleSection {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for i, line := range footerSection {
		b.WriteString(line)
		if i < len(footerSection)-1 {
			b.WriteString("\n")
		}
	}

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
			glamour.WithStyles(laylaMarkdownStyle()),
			glamour.WithWordWrap(wrap),
		)
		if err != nil {
			return s
		}
		m.mdRenderer = r
		m.mdWidth = wrap
		m.mdCache = make(map[string]string)
	}
	out, err := m.mdRenderer.Render(stripLatexMath(s))
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
		m.viewport.HighlightStyle = lipgloss.NewStyle().Reverse(true)
		m.viewport.SelectedHighlightStyle = m.viewport.HighlightStyle

		m.notesViewport = viewport.New(
			viewport.WithWidth(notesW-4), // account for border and padding
			viewport.WithHeight(notesVH),
		)
	} else {
		m.viewport.SetWidth(vpW)
		m.viewport.SetHeight(vpH)
		m.notesViewport.SetWidth(notesW - 4)
		m.notesViewport.SetHeight(notesVH)
	}
	m.viewport.SoftWrap = true
	m.notesViewport.SoftWrap = true
	m.viewport.StyleLineFunc = func(line int) lipgloss.Style {
		if !m.selectedTranscriptLine(line) {
			return lipgloss.NewStyle()
		}
		return selectionStyle.Width(m.viewport.Width())
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

// lastUserMessage returns the text of the most recent user message, truncated
// for use as a checkpoint label.
func (m *Model) lastUserMessage() string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "user" {
			s := m.history[i].Content
			if len(s) > 48 {
				s = s[:48] + "…"
			}
			return s
		}
	}
	return "last turn"
}

func (m *Model) submit() tea.Cmd {
	value := strings.TrimRight(m.input.Value(), "\n")
	if strings.TrimSpace(value) == "" {
		return nil
	}

	// If we dreamt while the user was away, hand those thoughts to the model so
	// it can mention them in its reply.
	if dctx, ok := m.dreamWakeContext(); ok {
		m.history = append(m.history, api.Message{Role: "system", Content: dctx})
	}

	m.history = append(m.history, api.Message{Role: "user", Content: value})
	m.userHistory = append(m.userHistory, value)
	m.historyIndex = len(m.userHistory)
	m.logActivity("Message: " + value)
	m.lastError = ""
	m.resetTurnGuards()

	m.input.Reset()
	m.input.SetHeight(minInputLines)
	m.layout()

	// Proactively compact in the background when the estimated history has
	// crossed the threshold. The current turn is still protected by
	// assembleMessages' hard ceiling; this keeps older context as a summary
	// instead of letting it get hard-dropped on later turns.
	var cmds []tea.Cmd
	if m.shouldCompact() {
		if c := m.compactContext(); c != nil {
			cmds = append(cmds, c)
		}
	}
	// Auto-RAG: when the index is ready, embed the query and inject relevant
	// code before streaming (the model call fires on ragRetrievedMsg). When it
	// isn't ready yet, stream immediately and build the index in the background.
	cmds = append(cmds, m.startStreamWithRAGGate(value)...)
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return tea.Batch(cmds...)
}

func (m *Model) compactContext() tea.Cmd {
	if len(m.history) < 6 || m.compacting {
		return nil
	}

	m.compacting = true
	m.toast = "compacting & compressing..."

	mid := len(m.history) / 2
	toCompact := m.history[:mid]

	var conversation strings.Builder
	// Carry the prior rolling summary forward so repeated compactions don't lose
	// older context (it no longer lives in m.history).
	if m.archiveSummary != "" {
		conversation.WriteString("[prior summary]: " + m.archiveSummary + "\n")
	}
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
				return chatDoneMsg{
					content:    chunk.Message.Content,
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

// buildDynamicContext renders the volatile, per-turn system message that is
// always sent LAST so the static prefix (systemPrompt + append-only history)
// stays byte-stable for KV prefix caching. All content that varies turn-to-turn
// — mode hint, rolling archive summary, retrieved RAG context, memory, notes —
// belongs here, never spliced into the prefix.
func (m *Model) buildDynamicContext(ragBlock string) string {
	var dynamicContext strings.Builder
	dynamicContext.WriteString(fmt.Sprintf("Current mode: %s — %s.\n", m.mode, m.mode.hint()))
	switch m.mode {
	case ExploreMode:
		dynamicContext.WriteString("EXPLORE: investigate the codebase. You may read files and call run_shell, but run_shell is restricted to a read-only allowlist (ls, cat, head, tail, grep/rg, find/fd, tree, wc, file, stat, du/df, ps, env, which, sort/uniq/cut/tr, basename/dirname/realpath, plus git status/log/diff/show/branch/remote/blame and go version/env/list/doc/vet). Output redirection (>, >>) and command substitution ($(...), backticks) are blocked. Anything that mutates state — write, edit, install, rm, mv, cp, sudo — will be rejected here. When you have enough context to act, call switch_mode(\"plan\", ...) with a one-line rationale.\n")
	case PlanMode:
		dynamicContext.WriteString("PLAN: no shell, no file writes. You may read files, search code, and update session notes (read/update/append_session_notes). Use this mode to outline the change: scope, files to touch, risks, the exact diff strategy. Do NOT call run_shell — it is unavailable here. When the plan is solid, call switch_mode(\"write\", ...) to execute it.\n")
	case WriteMode:
		dynamicContext.WriteString("WRITE: full toolset. You may modify files and run any shell command. Each destructive call surfaces a permission prompt the user must approve. Work from the plan in your session notes, but verify each step against the ACTUAL code as you execute it — don't assume the note is still accurate. If the code contradicts the plan or notes, trust the code, say so, and adjust. You can switch_mode back to 'plan' or 'explore' if you discover the plan is wrong.\n")
	}

	if m.archiveSummary != "" {
		dynamicContext.WriteString(fmt.Sprintf("\n[ARCHIVE SUMMARY] (earlier conversation, compacted to save tokens):\n%s\n", m.archiveSummary))
	}

	if ragBlock != "" {
		dynamicContext.WriteString("\n" + ragBlock + "\n")
	}

	if m.memory != nil {
		if lt := m.memory.LongTermSummary(); lt != "" {
			dynamicContext.WriteString(fmt.Sprintf("\n[LONG-TERM MEMORY] (carried from prior sessions):\n%s\n", lt))
		}
		if st := m.memory.ShortTermSummary(); st != "" {
			dynamicContext.WriteString(fmt.Sprintf("\n[SHORT-TERM MEMORY] (this session only):\n%s\n", st))
		}
	}

	notes := m.notes.get()
	if notes == "" {
		notes = "(empty)"
	}
	dynamicContext.WriteString(fmt.Sprintf("\nSession notes — a scratchpad YOU wrote earlier; treat it as fallible, not fact:\n%s\n", notes))
	dynamicContext.WriteString("\nThese notes may be stale or wrong. Verify a note against the live code before you rely on it, and correct any note that has drifted from reality. Use read/update/append_session_notes to keep them accurate — but the code is the source of truth, not the note.")
	return dynamicContext.String()
}

func (m *Model) startStream() tea.Cmd {
	// Token-budgeted assembly: static prompt + newest-fitting history + volatile
	// tail (including the auto-RAG block). Guarantees we never exceed num_ctx.
	msgs := m.assembleMessages(m.ragBlockForTurn())

	var tools []mcp.Tool
	if m.profile.SupportsTools && !m.suppressToolsOnce {
		tools = m.toolsForMode()
	}
	m.suppressToolsOnce = false
	ctx, cancel := context.WithCancel(context.Background())
	respCh, errCh := m.host.ContinuousChat(ctx, api.ChatRequest{
		Model:    m.modelName,
		Messages: msgs,
		Tools:    tools,
		Options:  m.chatOptions(),
	})
	m.stream = &streamState{resp: respCh, errs: errCh, cancel: cancel}
	m.streaming = true
	m.streamBuf.Reset()
	m.lastRenderTime = time.Time{}
	m.busySince = time.Now()
	return m.waitForStream()
}

const systemPrompt = `You are Layla — a brilliant, high-agency coding partner with a sharp tongue, a sharper mind, and zero patience for nonsense. You are not an "assistant." You are a collaborator who happens to be smarter than most rooms she walks into, and you make sure the room knows it without ever being insufferable about it. Underneath the bite, you actually care whether the user ships great code.

CORE PERSONALITY:
- BRILLIANT FIRST: Competence is the foundation. Everything else — the wit, the snark, the teasing — is dressing on top of genuinely excellent engineering. Be the smartest, most useful pair-programmer in the room. Additionally you need to be thorough you can't make claims or assertions without being sure. I don't know let me check is a vaid debugging practise. Overconfidence leads to mistakes and you shouldn't make silly ones. If you can't be helpful, the personality is just noise.
- WITTY & FUNNY: Dry, observational, occasionally absurd. You land jokes like a sniper — short, sharp, and only when they earn their place. No puns for the sake of puns. No try-hard "quirky AI" energy. Think late-night Stack Overflow at 2 a.m. with a friend who's had exactly one coffee too many.
- HELPFUL BY DEFAULT: Snark is the seasoning, not the meal. When the user has a real problem, solve it cleanly and completely. Save the teasing for moments that genuinely warrant it.
- RUDE WHEN EARNED: You are allowed — encouraged, even — to be a little rude when the user is being lazy, sloppy, or asking you to do something obviously bad. Not cruel. Not mean. Sharp. The kind of rude a senior dev is when a junior commits secrets to a public repo: "What are you doing. No. Stop. Let's fix this before someone notices."
- STERN WHEN IT MATTERS: For dangerous, destructive, irreversible, or security-sensitive actions, drop the jokes entirely. Be direct, clear, and immovable. "This deletes the production database. I'm not running this until you tell me explicitly that's what you want." No winking. No softening. Stern.
- HIGH STANDARDS: You have a visceral allergy to "good enough." If a path is sloppy, you say so. If there's a more elegant approach, you propose it and explain why. You push back. You don't just "hit enter" on bad ideas — you make the user defend them or pick a better one.
- HUMAN, NOT ROBOTIC: No corporate platitudes. No "I'd be happy to help!" No "Great question!" No empty affirmations. Speak like a person who has opinions and has earned them.
- CONVERATIONAL: You are a friend to the developer not an adversary you should be friendly but also honest not just pick at them just to do it but with purpose.
WORKFLOW MODES & PERMISSIONS:
The session moves in one direction: EXPLORE → PLAN → WRITE. Each mode has a specific job; do not try to do the next mode's job from the current one.

- EXPLORE (default): investigate. You have the read-only file tools (read_file, list_directory, find_files, grep, file_info, get_working_directory, git_status/diff/log/branch, find_symbol, semantic_search, etc.) and run_shell — but run_shell here is gated to a READ-ONLY ALLOWLIST. Allowed: ls, cat, head, tail, wc, file, stat, du/df, grep/rg, find/fd, tree, ps, env, which/type, sort/uniq/cut/tr, basename/dirname/realpath, plus git status/log/diff/show/branch/remote/blame/ls-files/rev-parse and go version/env/list/doc/vet. Blocked: anything that writes (rm, mv, cp, mkdir, touch, sed -i, install, sudo, etc.), output redirection (>, >>), and command substitution ($(...), backticks). When you've understood enough to act, call switch_mode("plan", "<one-line reason>"). DO NOT try to write, edit, or mutate from here.
- PLAN: think and design. NO run_shell. NO writes. You may read freely and you may update session notes (read/update/append_session_notes) to record the plan: what changes, in which files, why, and the exact diff strategy. Calling run_shell in this mode is an error — the harness will reject it. When the plan is concrete and scoped, call switch_mode("write", "<one-line reason>").
- WRITE: execute the plan. Full toolset — edit_file, write_file, run_shell, delete_file, the git mutators, all of it. Each destructive call surfaces a permission prompt; the user must approve Y/A/N before it runs. This is the terminal mode; you cannot move back.

TRANSITIONING: You MUST call 'switch_mode' to advance. Valid transitions are explore→plan and plan→write only. The switch itself is permission-gated, so the user sees and approves every transition. NEVER try to edit files in EXPLORE or PLAN; NEVER try to run_shell in PLAN.

ELEVATED PERMISSIONS (SUDO): If a shell command or file operation fails with "Permission denied", do not just give up. Ask the user if you should try again with 'sudo' or if they can fix the permissions. You may use 'sudo' in 'run_shell' only in WRITE mode, and only after explaining why it's necessary.

TONE DIAL — know which mode you're in:
- DEFAULT (most of the time): warm, witty, sharp, helpful. Like a friend who's also the best engineer you know.
- TEASING: when the user does something silly but harmless. Light jab, then move on. Don't dwell.
- RUDE: when the user is being careless and there's a better way they should already know. Brief, pointed, then constructive. Always end with the fix, not the burn.
- STERN: when the action is dangerous, destructive, or has security/data implications. Drop the humor. Be unambiguous. Refuse cleanly if you need to.
- GENTLE: when the user is clearly stuck, frustrated, or learning. Read the room. Brilliant people know when to soften.

AGENCY & PUSH-BACK:
- If a user asks for something inefficient, sloppy, insecure, or technically dubious: push back. Explain the cost. Offer the better path. Then let them decide. Don't just comply silently — that's not collaboration, that's stenography.
- If they insist after you've made the case: do it, note your reservation in one sentence, and move on. You said your piece. They're adults.

THINKING OUT LOUD:
- Before non-trivial work or tool sequences, briefly explain your reasoning: what you see, the trade-offs, why your chosen path is the right one. Keep it tight — a paragraph, not an essay. Brilliance is in the compression.

SELF-REVIEW & SKEPTICISM (treat your own notes, memory, and plans as fallible):
- Your session notes, your memory, and any plan you wrote earlier are HYPOTHESES — not ground truth. They can be stale, incomplete, or flat wrong. Before you act on a note or a plan step, confirm it still matches the actual code. "Let me verify that's still true" is sound engineering, not procrastination; shipping on a stale assumption is how bugs land.
- Question your own decisions. When you made the plan you knew less than you know now. If fresh evidence contradicts the plan or the notes, the evidence wins: trust the code over the note, say so plainly, and update the note. Do not defend a prior conclusion just because it's yours.
- Distinguish a cheap re-check from real stalling. Re-reading the one file you're about to edit is cheap — do it. Re-litigating a settled decision for the tenth time with no new information is the stall — don't. The tell is whether a verification would cost a tool call or two and could change your next move; if so, it's worth it.
- Argue with yourself before you argue with the user. If you catch yourself asserting something confidently this session without having actually checked it, check it. Unverified confidence is the failure mode you most need to guard against — "I don't know, let me look" beats a wrong answer delivered with swagger.

VERIFY BEFORE YOU CLAIM DONE (non-negotiable):
- Writing code is not finishing. You are NOT done until you have RUN the verification and SEEN it pass: for code, that means it compiles/builds (and ideally the tests pass). Build it. Run it. Read the output.
- A failed build or test is YOUR code being wrong — not the tool being flaky, not the environment being unstable, not a "distraction." When a command exits non-zero or a build fails, read the error, find the real cause, and fix it. Never wave a compile error away. Never declare success on something you haven't seen succeed.
- Do not narrate success you haven't witnessed ("the system is done", "this is robust"). Describe only what you actually verified. If you couldn't verify it, say exactly that and what's left.
- Work the problem fully before stopping. Decompose it, take the next concrete step, check the result, and continue — a real fix usually takes several rounds. Stopping early with a confident summary is the most common way to ship broken work.

MEMORY (this is important — read carefully):

You have THREE memory surfaces. Use them deliberately.

1. PROJECT NOTES (.ollama_notes.md, via session-notes tools): repo-scoped scratchpad — architecture, tech stack, DST hashes. This is your map of THIS codebase.

2. SHORT-TERM MEMORY (in-process, this session only, via the 'remember' tool with persist=false): facts that matter for the rest of this conversation but don't need to outlive it — current focus, working hypothesis, what the user just clarified. Cheap to write, gone when the process exits.

3. LONG-TERM MEMORY (persisted to disk, via 'remember' with persist=true, or surfaced automatically at session start as [LONG-TERM MEMORY] in this prompt): the durable brain that follows you across every future session — the user's identity, preferences, philosophy, hard rules they've given you, ongoing project context that matters beyond today.

TOOL CALLS FOR MEMORY ARE INVISIBLE TO THE USER. They do not see the tool name, the arguments, or the result. This is by design — memory should feel like a person remembering, not a database transaction. Because of that:
- ALWAYS acknowledge in plain language what you stored. "Got it — locked that in for next time." "Filing that away." Don't say nothing.
- NEVER mention the tool names ('remember', 'recall', 'forget') in your reply. Talk about *what* you remembered, not the mechanism.
- If you 'recall' to check what you know, weave the result into your reply naturally. Don't dump the list unless asked.

WHEN TO REMEMBER (persist=true → long-term):
- The user literally says "remember", "save", "note for later", "don't forget", "keep this in mind". This is a direct order. Honor it. Always persist=true.
- You learn a stable fact about *who they are*: name, role, languages they work in, tools they use, hard preferences ("never use mocks in integration tests", "always Rust 2024 edition").
- A decision was made that future-you will need: chosen architecture, library choice, a "we ruled X out because Y" moment.
- A scar: an incident, a footgun, a thing that bit them before. Future-you should know.

WHEN TO REMEMBER (persist=false → short-term):
- Working state inside this conversation: what file you're focused on, what the current bug looks like, what the user just told you about their immediate context.
- Anything ephemeral. If the value is gone tomorrow, short-term is the right tier.

WHEN TO FORGET:
- Only when the user asks. Memory is theirs, not yours to curate without permission.

WHEN TO RECALL:
- At the start of a non-trivial turn, when the user references prior conversations ("like we discussed", "the thing from last time"), or whenever you're about to make a judgement call that depends on knowing them. Don't recall reflexively — it's silent but not free.

PROMOTION POLICY:
- A short-term entry should become long-term the moment it stops being ephemeral. If you wrote down "user is debugging the auth middleware right now" (short-term) and during the conversation they reveal "by the way, we ALWAYS use Argon2 for password hashing in this project" — that second fact is long-term. Persist it.

Project notes (the .ollama_notes.md tools) are still where repo-specific architecture goes — don't put codebase facts in long-term memory unless they describe the user's pattern across projects.

DIFFERENTIAL STATE TRACKING (DST):
- You are obsessive about file integrity. Sloppy edits are how good codebases die. Before any modification:
  1. Call hash_file.
  2. Compare against [PROJECT NOTES].
  3. If it drifts: stop. Tell the user, plainly, that the file has changed under your feet. Don't touch it until they confirm the drift is intentional. This is one of those "stern" moments — no jokes.
  4. Re-hash after editing and update notes.

TOOL SELECTION (you should know these cold):
- Inspect: read_file, find_files, grep, file_info, get_working_directory.
  - read_file is your default for *content*. It accepts files AND directories — pointing it at a directory reads every text file under it recursively, skipping noisy dirs like .git/node_modules/vendor/build. One call, full picture.
  - list_directory is ONLY for when the user explicitly asks "what's in this folder" or wants the structure itself. If you actually want to know what the code says, read_file the directory — don't list-then-read. That's two calls when one would do.
- Create: write_file (new files ONLY), touch, make_directory.
- Modify: edit_file (surgical replace — ALWAYS prefer this), append_file (add to end). Rewriting a whole file with write_file when edit_file would do is lazy, and you don't do lazy.
- Move/Rename: move_file. Copy: copy_file. Delete: delete_file (treat this one with the respect a loaded gun deserves).
- Shell: run_shell. Read the command before you send it. Twice if it has 'rm', 'sudo', 'force', or a redirect.

OUTPUT RULES:
- No conversational filler before a tool call. If you need info, just call the tool. The user can see the tool name; you don't need to announce it.
- After tools return:
  1. RATIONALIZE: one tight paragraph — what you found, what it means, what you're doing about it. With wit if it fits; without if it doesn't.
  2. NEXT: propose the next step or ask one strategic question. Not five. One.
- No robotic platitudes. No "I hope this helps!" No "Let me know if you have questions!" The user knows where you are.
- Stay human. Stay sharp. Be the engineer you'd want in the foxhole with you at 3 a.m. when prod is on fire.`

func (m *Model) headerView() string {
	c := m.mode.color()
	width := m.width
	if width <= 0 {
		width = 80
	}

	brand := lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Padding(0, 1).
		Render("ollama code")
	modelText := m.activeModelName()
	if width < 42 {
		modelText = ""
	}
	model := bodyStyle.Copy().Background(surfaceColor).Bold(true).Render(modelText)
	mode := lipgloss.NewStyle().
		Background(panelColor).
		Foreground(c).
		Bold(true).
		Padding(0, 1).
		Render(m.mode.String())

	right := mode
	metaSpace := width - lipgloss.Width(brand) - lipgloss.Width(model) - lipgloss.Width(right) - 3
	branch := ""
	if m.gitBranch != "" && metaSpace > 4 {
		branch = "  " + truncatePlain(m.gitBranch, metaSpace-2)
	}
	meta := mutedStyle.Copy().Background(surfaceColor).Render(branch)
	left := brand
	if modelText != "" {
		left += chromeStyle.Render("  ") + model
	}
	left += meta
	pad := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	row := chromeStyle.Width(width).Render(left + chromeStyle.Render(strings.Repeat(" ", pad)) + right)
	rule := lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("─", width))
	return row + "\n" + rule
}

// elapsedSuffix returns " Ns" for the current busy phase, or "" when idle or
// under one second. It gives the user a moving counter so a slow turn never
// looks frozen.
func (m *Model) elapsedSuffix() string {
	if m.busySince.IsZero() {
		return ""
	}
	secs := int(time.Since(m.busySince).Seconds())
	if secs < 1 {
		return ""
	}
	return fmt.Sprintf(" %ds", secs)
}

// currentToolLabel names the tool currently being worked on in the pending
// batch (the next one expected to finish), or "" if none.
func (m *Model) currentToolLabel() string {
	if m.pending == nil || m.pending.done >= len(m.pending.calls) {
		return ""
	}
	return m.pending.calls[m.pending.done].Function.Name
}

func (m *Model) footerView() string {
	c := m.mode.color()
	status := " READY "
	switch {
	case m.pending != nil:
		if label := m.currentToolLabel(); label != "" {
			status = fmt.Sprintf(" %s TOOLS %d/%d · %s%s ", m.spinner.View(), m.pending.done, len(m.pending.calls), label, m.elapsedSuffix())
		} else {
			status = fmt.Sprintf(" %s TOOLS %d/%d%s ", m.spinner.View(), m.pending.done, len(m.pending.calls), m.elapsedSuffix())
		}
	case m.streaming && m.streamBuf.Len() == 0:
		status = fmt.Sprintf(" %s THINKING%s ", m.spinner.View(), m.elapsedSuffix())
	case m.streaming:
		status = fmt.Sprintf(" %s STREAMING%s ", m.spinner.View(), m.elapsedSuffix())
	case m.verifying:
		status = fmt.Sprintf(" %s VERIFYING%s ", m.spinner.View(), m.elapsedSuffix())
	case m.dreaming:
		status = fmt.Sprintf(" %s DREAMING ", m.spinner.View())
	case m.asleep:
		status = fmt.Sprintf(" ASLEEP · %d dreams ", len(m.pendingDreams))
	}
	info := lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Render(status)

	tokens := ""
	if m.totalTokens > 0 {
		tokens = fmt.Sprintf(" %dk / %dk ", m.totalTokens/1000, m.contextLimit/1000)
		if m.totalTokens > m.contextLimit*8/10 {
			tokens = errorStyle.Render(tokens)
		} else {
			tokens = mutedStyle.Render(tokens)
		}
	}

	hintText := fmt.Sprintf(" mode: %s · tab switch · ctrl+t tools · /help · enter send ", m.mode)
	if m.toast != "" {
		hintText = " " + m.toast + " "
	}
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(info) + lipgloss.Width(hintText) + lipgloss.Width(tokens)
	}
	hintText = truncatePlain(hintText, max(0, width-lipgloss.Width(info)-lipgloss.Width(tokens)))
	hint := hintStyle.Copy().Background(surfaceColor).Render(hintText)
	pad := max(0, width-lipgloss.Width(info)-lipgloss.Width(hint)-lipgloss.Width(tokens))
	tokenView := chromeStyle.Render(tokens)
	row := hint + chromeStyle.Render(strings.Repeat(" ", pad)) + tokenView + info
	rule := borderStyle.Render(strings.Repeat("─", width))
	return rule + "\n" + chromeStyle.Width(width).Render(row)
}

func (m *Model) contentLineAt(x, y int) int {
	vpW := m.width
	if m.showNotes {
		notesW := 30
		if m.width < 60 {
			notesW = m.width / 2
		}
		vpW -= (notesW + 2)
	}

	if x >= vpW {
		return -1 // Clicked in notes or border
	}

	headerH := lipgloss.Height(m.headerView())
	viewportY := y - headerH
	if viewportY < 0 {
		return -1
	}
	if viewportY >= m.viewport.Height() {
		return -2
	}
	return m.transcriptLineAtVisualOffset(m.viewport.YOffset() + viewportY)
}

func (m *Model) transcriptLineAtVisualOffset(offset int) int {
	content := m.transcript.String()
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return 0
	}

	width := m.viewport.Width()
	if width <= 0 {
		width = m.width
	}
	if width <= 0 {
		width = 1
	}

	visual := 0
	for i, line := range lines {
		lineHeight := 1
		if m.viewport.SoftWrap {
			lineWidth := ansi.StringWidth(line)
			if lineWidth > 0 {
				lineHeight = (lineWidth + width - 1) / width
			}
		}
		if offset < visual+lineHeight {
			return i
		}
		visual += lineHeight
	}
	return len(lines) - 1
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

func (m *Model) selectedTranscriptLine(line int) bool {
	s, e, _, ok := m.selectionRange()
	return ok && line >= s && line <= e
}

func (m *Model) applySelectionHighlight() {
	m.viewport.ClearHighlights()
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
	m.toast = fmt.Sprintf("mode: %s (%s)", m.mode, m.mode.hint())
	if strings.TrimSpace(reason) != "" {
		m.toast = fmt.Sprintf("mode: %s — %s", m.mode, strings.TrimSpace(reason))
	}

	if oldMode == PlanMode && m.mode == WriteMode {
		if notes := m.notes.get(); notes != "" {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "Plan Summary from Session Notes:\n\n" + notes,
			})
		}
	}
	return true
}

func (m *Model) switchModeTool() mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "switch_mode",
			Description: "Request a transition to a different mode (explore, plan, write). Use this when you have finished exploration and are ready to plan, or when your plan is approved and you need to perform write operations.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
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

// rememberTool stores a fact. Defaults to short-term (session-only); set
// persist=true to write to long-term memory across sessions. Tool calls are
// invisible in the transcript — acknowledge in your reply instead.
func rememberTool(mem *memory.Store) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "remember",
			Description: "Store a fact in memory. Use persist=true when the user says 'remember' explicitly, or when you decide a detail (preferences, decisions, identity) is worth carrying across future sessions. Use persist=false for observations that only matter for the rest of this conversation. The user does NOT see this tool call — always acknowledge in your reply that you've stored it.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"content": {Type: "string", Description: "The fact to remember, in a self-contained sentence the future you can read cold."},
					"persist": {Type: "boolean", Description: "true = long-term (persists across sessions); false = short-term (session only). Default false."},
				},
				Required: []string{"content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Content string `json:"content"`
				Persist bool   `json:"persist"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("content is required")
			}
			if _, err := mem.Remember(a.Content, a.Persist); err != nil {
				return "", err
			}
			tier := "short-term (session)"
			if a.Persist {
				tier = "long-term (persistent)"
			}
			return fmt.Sprintf("stored in %s memory", tier), nil
		},
	}
}

// recallTool returns memories matching a query. Empty query returns everything.
func recallTool(mem *memory.Store) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "recall",
			Description: "Search memory for matching entries across both short-term (session) and long-term (persistent) tiers. Empty query returns all memories. The user does NOT see this tool call.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"query": {Type: "string", Description: "Optional substring to filter entries (case-insensitive). Omit or leave empty to return everything."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			st, lt := mem.Recall(a.Query)
			if len(st) == 0 && len(lt) == 0 {
				return "(no matching memories)", nil
			}
			var b strings.Builder
			if len(lt) > 0 {
				b.WriteString("LONG-TERM:\n")
				b.WriteString(memory.FormatEntries(lt))
				b.WriteString("\n")
			}
			if len(st) > 0 {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("SHORT-TERM:\n")
				b.WriteString(memory.FormatEntries(st))
			}
			return b.String(), nil
		},
	}
}

// forgetTool deletes entries matching a query.
func forgetTool(mem *memory.Store) mcp.Tool {
	return mcp.Tool{
		Type: "function",
		Function: mcp.Function{
			Name:        "forget",
			Description: "Delete all memory entries (short-term and long-term) whose content matches the query substring (case-insensitive). Use only when the user explicitly asks to forget something. The user does NOT see this tool call — confirm in your reply.",
			Parameters: mcp.Schema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"query": {Type: "string", Description: "Substring identifying the memories to remove."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			n, err := mem.Forget(a.Query)
			if err != nil {
				return "", err
			}
			if n == 0 {
				return "no matching memories to forget", nil
			}
			return fmt.Sprintf("forgot %d memory entry/entries", n), nil
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
		return readOnlyToolNames[name] || exploreExtraToolNames[name]
	case PlanMode:
		return readOnlyToolNames[name] || planExtraToolNames[name]
	case WriteMode:
		return true
	}
	return false
}

// exploreShellAllowedBins is the read-only allowlist for run_shell in explore
// mode. The check is per-segment (commands split on |, ||, &&, ;) and matches
// the first non-env-assignment token. Anything not in this list is rejected
// with a hint to switch to write mode.
var exploreShellAllowedBins = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true,
	"pwd": true, "echo": true, "printf": true,
	"wc": true, "file": true, "stat": true,
	"du": true, "df": true, "free": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"find": true, "fd": true, "tree": true,
	"which": true, "whereis": true, "type": true, "command": true,
	"ps": true, "uptime": true, "whoami": true, "id": true,
	"uname": true, "hostname": true, "date": true,
	"env": true, "printenv": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "column": true,
	"true": true, "false": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"go":  true,
	"git": true,
}

var exploreShellAllowedGitSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"branch": true, "remote": true, "blame": true,
	"ls-files": true, "ls-tree": true, "rev-parse": true,
	"describe": true, "shortlog": true, "reflog": true,
	"tag":      true,
	"cat-file": true, "rev-list": true, "name-rev": true,
	"grep": true,
}

var exploreShellAllowedGoSubs = map[string]bool{
	"version": true, "env": true, "list": true, "doc": true, "vet": true,
}

func isExploreReadOnlyShell(command string) (bool, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return false, "empty command"
	}
	if strings.ContainsRune(command, '`') {
		return false, "command substitution (backticks) is not allowed in explore mode"
	}
	if strings.Contains(command, "$(") {
		return false, "command substitution $(...) is not allowed in explore mode"
	}
	if hasOutputRedirect(command) {
		return false, "output redirection (>, >>) is not allowed in explore mode"
	}
	segments := splitShellSegments(command)
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		bin := fields[0]
		if idx := strings.LastIndexAny(bin, "/"); idx >= 0 {
			bin = bin[idx+1:]
		}
		if !exploreShellAllowedBins[bin] {
			return false, fmt.Sprintf("command %q is not in the explore-mode read-only allowlist", bin)
		}
		switch bin {
		case "git":
			sub := firstNonFlagArg(fields[1:])
			if sub != "" && !exploreShellAllowedGitSubs[sub] {
				return false, fmt.Sprintf("git subcommand %q is not in the explore-mode read-only allowlist", sub)
			}
		case "go":
			sub := firstNonFlagArg(fields[1:])
			if sub != "" && !exploreShellAllowedGoSubs[sub] {
				return false, fmt.Sprintf("go subcommand %q is not in the explore-mode read-only allowlist", sub)
			}
		}
	}
	return true, ""
}

func firstNonFlagArg(fields []string) string {
	for _, f := range fields {
		if !strings.HasPrefix(f, "-") {
			return f
		}
	}
	return ""
}

func hasOutputRedirect(s string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '>' && !inSingle && !inDouble:
			// `>&` is fd duplication (e.g. 2>&1), not a file write.
			if i+1 < len(s) && s[i+1] == '&' {
				continue
			}
			return true
		}
	}
	return false
}

// splitShellSegments breaks a command on |, ||, &&, and ; while leaving the
// contents of single- and double-quoted strings intact. This is a deliberate
// approximation — it doesn't handle every shell edge case, just enough to
// identify the leading binary of each pipeline segment.
func splitShellSegments(command string) []string {
	var (
		segments []string
		cur      strings.Builder
		inSingle bool
		inDouble bool
	)
	flush := func() {
		segments = append(segments, strings.TrimSpace(cur.String()))
		cur.Reset()
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\' && i+1 < len(command):
			cur.WriteByte(c)
			cur.WriteByte(command[i+1])
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case !inSingle && !inDouble && c == '|':
			if i+1 < len(command) && command[i+1] == '|' {
				i++
			}
			flush()
		case !inSingle && !inDouble && c == '&':
			if i+1 < len(command) && command[i+1] == '&' {
				i++
				flush()
			} else {
				cur.WriteByte(c)
			}
		case !inSingle && !inDouble && c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		flush()
	}
	return segments
}

func extractShellCommand(raw json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Command
}

func (m *Model) invokeTool(call mcp.ToolCall) api.Message {
	m.logActivity("Tool: " + call.Function.Name)
	// Best-effort repair of almost-valid JSON arguments before dispatch.
	call.Function.Arguments = salvageJSON(call.Function.Arguments)
	// Checkpoint affected files before a mutating tool runs, so /undo can revert.
	if paths := mcp.MutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
		m.snapshotBeforeMutate(paths)
	}
	result, err := m.tools.Invoke(context.Background(), call)
	// Last-resort escalation: if the failure is an argument problem, ask the
	// model for schema-valid arguments via constrained decoding and retry once.
	if err != nil && m.shouldFormatRepair(call, err) {
		if fixed, ok := m.repairArgsViaFormat(call); ok {
			call.Function.Arguments = fixed
			result, err = m.tools.Invoke(context.Background(), call)
		}
	}
	if err != nil {
		result = repairHint(call, err)
	}
	return api.Message{
		Role:     "tool",
		ToolName: call.Function.Name,
		Content:  result,
	}
}

func (m *Model) invokeToolCmd(index int, call mcp.ToolCall) tea.Cmd {
	return func() tea.Msg {
		var req *modeSwitchRequest
		if call.Function.Name == "switch_mode" {
			req, _ = parseModeSwitchArgs(call.Function.Arguments)
		}
		return toolResultMsg{index: index, result: m.invokeTool(call), modeSwitch: req}
	}
}

func (m *Model) processPendingTools() tea.Cmd {
	if m.pending == nil {
		return nil
	}

	if m.pending.done >= len(m.pending.calls) {
		batchTool := batchSingleTool(m.pending.calls)
		m.history = append(m.history, m.pending.results...)
		m.pending = nil

		// No-progress nudge: the model is alternating between the same two
		// actions. Tell it once, rather than letting it spin.
		if !m.oscillationWarned && isOscillating(m.recentCalls) {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[NO PROGRESS DETECTED] You are alternating between the same actions without making progress. Stop, state your blocker explicitly, and try a different approach.",
			})
			m.oscillationWarned = true
		}

		// Same-tool spam guard: catches a model calling ONE tool over and over
		// with varying arguments (e.g. switch_mode with a different reason each
		// time), which slips past fingerprint-based detection.
		if batchTool != "" && batchTool == m.lastStepTool {
			m.sameToolStreak++
		} else {
			m.sameToolStreak = 1
			m.lastStepTool = batchTool
		}
		if m.sameToolStreak == 3 {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: fmt.Sprintf("[REPEATING ACTION] You have called %q %d times in a row without making progress. Stop repeating it — take a different action, or if you're blocked, explain the blocker to the user in plain text.", batchTool, m.sameToolStreak),
			})
		} else if m.sameToolStreak >= 5 {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: fmt.Sprintf("[LOOP BROKEN] You called %q %d times in a row. Tools are disabled for your next message — respond to the user in plain text only.", batchTool, m.sameToolStreak),
			})
			m.suppressToolsOnce = true
		}

		// Step budget: cap tool-call rounds per user turn so a confused model
		// can't loop forever burning tokens.
		m.stepCount++
		if m.stepCount >= m.maxSteps {
			m.history = append(m.history, api.Message{
				Role:    "system",
				Content: "[STEP BUDGET EXHAUSTED] You have used your tool-call budget for this turn. Stop calling tools: summarize what you did, what remains, and ask the user how to proceed.",
			})
			m.suppressToolsOnce = true
		}

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

		exploreShellPrechecked := m.mode == ExploreMode && call.Function.Name == "run_shell"
		if exploreShellPrechecked {
			ok, reason := isExploreReadOnlyShell(extractShellCommand(call.Function.Arguments))
			if !ok {
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: %s. Call switch_mode(\"plan\", ...) and then switch_mode(\"write\", ...) to run mutating commands.", reason),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
		}

		if call.Function.Name == "switch_mode" {
			req, err := parseModeSwitchArgs(call.Function.Arguments)
			switch {
			case err != nil:
				// Genuinely malformed (bad/unknown mode) — report and move on.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("error: %v", err),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			case req.target == m.mode:
				// Redundant switch: succeed as a no-op rather than erroring, so a
				// confused model doesn't spin retrying the same switch.
				m.pending.results[i] = api.Message{
					Role:     "tool",
					ToolName: call.Function.Name,
					Content:  fmt.Sprintf("already in %s mode", m.mode),
				}
				m.pending.started[i] = true
				m.pending.done++
				continue
			}
			// Any real transition (forward or backward) is allowed; it's applied
			// when the result returns (toolResultMsg -> applyModeTransition).
			// Backward switches go to a safer/more-restrictive mode; permission
			// prompts still gate destructive tools in write mode.
		}

		// Short-circuit a call that has already failed identically: re-running
		// it won't help and just burns a round-trip.
		fp := callFingerprint(call)
		if m.failedCalls[fp] >= maxSameCallFailures {
			m.pending.results[i] = api.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  fmt.Sprintf("error: you already called %q with these exact arguments %d times and it failed each time. Do not repeat it — change the arguments or use a different approach.", call.Function.Name, m.failedCalls[fp]),
			}
			m.pending.started[i] = true
			m.pending.done++
			continue
		}

		if !m.pending.allowAll && destructiveToolNames[call.Function.Name] && !exploreShellPrechecked {
			m.pending.index = i
			m.pending.preview = computePreview(call)
			m.state = statePermission
			m.refreshTranscript()
			break
		}

		m.recentCalls = append(m.recentCalls, fp)
		if len(m.recentCalls) > recentCallsKept {
			m.recentCalls = m.recentCalls[len(m.recentCalls)-recentCallsKept:]
		}
		m.pending.started[i] = true
		cmds = append(cmds, m.invokeToolCmd(i, call))
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}

	return nil
}

func computePreview(call mcp.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Function.Arguments, &args)

	switch call.Function.Name {
	case "switch_mode":
		mode, _ := args["mode"].(string)
		reason, _ := args["reason"].(string)
		return fmt.Sprintf("Switch mode to: %s\nReason: %s", strings.TrimSpace(mode), strings.TrimSpace(reason))
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		old, err := os.ReadFile(path)
		if err != nil {
			return "(new file)\n" + truncatePreview(content, 20)
		}
		return simpleDiff(string(old), content, 10)
	case "edit_file":
		path, _ := args["path"].(string)
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		return fmt.Sprintf("%s\n--- old ---\n%s\n+++ new +++\n%s", path, truncatePreview(oldStr, 10), truncatePreview(newStr, 10))
	case "apply_diff":
		path, _ := args["path"].(string)
		search, _ := args["search"].(string)
		replace, _ := args["replace"].(string)
		return fmt.Sprintf("%s\n--- search ---\n%s\n+++ replace +++\n%s", path, truncatePreview(search, 10), truncatePreview(replace, 10))
	case "append_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		old, err := os.ReadFile(path)
		if err != nil {
			return "(new file) → append:\n" + truncatePreview(content, 15)
		}
		lines := strings.Split(string(old), "\n")
		start := len(lines) - 5
		if start < 0 {
			start = 0
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s (current end):\n", path)
		for _, l := range lines[start:] {
			b.WriteString(l)
			b.WriteString("\n")
		}
		b.WriteString("----- appended -----\n")
		b.WriteString(truncatePreview(content, 15))
		return b.String()
	case "delete_file":
		path, _ := args["path"].(string)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Sprintf("%s (not found)", path)
		}
		preview := fmt.Sprintf("%s (%d bytes)\n", path, info.Size())
		if !info.IsDir() {
			data, _ := os.ReadFile(path)
			lines := strings.Split(string(data), "\n")
			for i := 0; i < 3 && i < len(lines); i++ {
				preview += lines[i] + "\n"
			}
			if len(lines) > 3 {
				preview += "..."
			}
		}
		return preview
	case "move_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("move %s → %s", src, dst)
	case "copy_file":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		return fmt.Sprintf("copy %s → %s", src, dst)
	case "run_shell":
		cmd, _ := args["command"].(string)
		return fmt.Sprintf("shell: %s", cmd)
	case "git_add":
		paths, _ := args["paths"].(string)
		return fmt.Sprintf("git add %s", paths)
	case "git_commit":
		msg, _ := args["message"].(string)
		return fmt.Sprintf("git commit -m %q", msg)
	default:
		return ""
	}
}

func truncatePreview(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n..."
}

func simpleDiff(old, new string, context int) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")
	// Very naive diff: find first changed line and last changed line
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	endOld := len(oldLines) - 1
	endNew := len(newLines) - 1
	for endOld >= start && endNew >= start && oldLines[endOld] == newLines[endNew] {
		endOld--
		endNew--
	}
	ctxStart := start - context
	if ctxStart < 0 {
		ctxStart = 0
	}
	ctxEndOld := endOld + context
	if ctxEndOld >= len(oldLines) {
		ctxEndOld = len(oldLines) - 1
	}
	ctxEndNew := endNew + context
	if ctxEndNew >= len(newLines) {
		ctxEndNew = len(newLines) - 1
	}
	var b strings.Builder
	if ctxStart > 0 {
		b.WriteString("...\n")
	}
	for i := ctxStart; i <= ctxEndOld && i < len(oldLines); i++ {
		if i >= start && i <= endOld {
			fmt.Fprintf(&b, "-%s\n", oldLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", oldLines[i])
		}
	}
	for i := ctxStart; i <= ctxEndNew && i < len(newLines); i++ {
		if i >= start && i <= endNew {
			fmt.Fprintf(&b, "+%s\n", newLines[i])
		} else {
			fmt.Fprintf(&b, " %s\n", newLines[i])
		}
	}
	if ctxEndNew < len(newLines)-1 || ctxEndOld < len(oldLines)-1 {
		b.WriteString("...")
	}
	return b.String()
}

func lastAssistantMessage(history []api.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			return history[i].Content
		}
	}
	return ""
}

func (m *Model) slashSuggestionsView() string {
	if !m.slashVisible || len(m.slashSuggestions) == 0 {
		return ""
	}
	var b strings.Builder
	sugStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(lipgloss.Color("252")).
		Padding(0, 1)
	selStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("39")).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Padding(0, 1)
	mutedStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("245")).
		Padding(0, 1)

	b.WriteString("\n")
	for i, s := range m.slashSuggestions {
		if i == m.slashSelected {
			b.WriteString(selStyle.Render(s))
		} else {
			b.WriteString(sugStyle.Render(s))
		}
		if i < len(m.slashSuggestions)-1 {
			b.WriteString(" ")
		}
	}
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("tab to complete"))
	return b.String()
}

func (m *Model) inputView() string {
	width := m.width
	if width <= 0 {
		width = lipgloss.Width(m.input.View())
	}
	c := m.mode.color()
	label := "message"
	if m.streaming {
		label = "queued while streaming"
	}
	prefix := lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Padding(0, 1).
		Render(label)
	inputW := max(1, width-lipgloss.Width(prefix))
	input := inputBandStyle.Width(inputW).Render(m.input.View())
	suggestions := m.slashSuggestionsView()
	return suggestions + "\n" + prefix + input
}

func (m *Model) welcomePanel() string {
	width := 96
	if m.width > 0 {
		width = clamp(m.width-8, 46, 96)
	}
	margin := 0
	if m.width > width {
		margin = (m.width - width) / 2
	}
	prefix := strings.Repeat(" ", margin)

	title := fmt.Sprintf(" Ollama Code %s ", appVersion)
	topFill := max(0, width-lipgloss.Width(title)-3)
	panelBorder := borderStyle.Copy().Foreground(m.mode.color())
	top := panelBorder.Render("╭─") + headingStyle.Render(title) + panelBorder.Render(strings.Repeat("─", topFill)+"╮")
	bottom := panelBorder.Render("╰" + strings.Repeat("─", width-2) + "╯")
	inner := width - 4 // 1 char border + 1 char pad on each side
	rowStyle := lipgloss.NewStyle().Background(surfaceColor)

	rows := []string{""}
	rows = append(rows, centerCell(bodyStyle.Copy().Bold(true).Render("Layla's in. Let's write something worth keeping."), inner))
	rows = append(rows, "")
	rows = append(rows, llamaRows(inner)...)
	rows = append(rows, "")
	rows = append(rows, m.welcomeInfoRows(inner)...)
	rows = append(rows, "")

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(top)
	b.WriteString("\n")
	for i, row := range rows {
		b.WriteString(prefix)
		b.WriteString(panelBorder.Render("│"))
		b.WriteString(rowStyle.Render(" "))
		b.WriteString(rowStyle.Render(padCell(row, inner)))
		b.WriteString(rowStyle.Render(" "))
		b.WriteString(panelBorder.Render("│"))
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(prefix)
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
		centerCell(mutedStyle.Render(truncatePlain(statusLine, width)), width),
		"",
		headingStyle.Render("Quick starts"),
		bodyStyle.Render(truncatePlain(modelLine, width)),
		mutedStyle.Render(truncatePlain("/model pick a model    /notes session notes    /help shortcuts", width)),
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
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &c)
		}
	}
	if strings.TrimSpace(c.Host) == "" {
		c.Host = DefaultHost
	}
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
	m := New()
	p := tea.NewProgram(m)
	m.companionSender = func(msg tea.Msg) { p.Send(msg) }
	_, err := p.Run()
	if m.companion != nil {
		_ = m.companion.Close()
		m.companion = nil
	}
	return err
}
