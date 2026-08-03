package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/companion"
	"github.com/javanhut/ollama_code/internal/memory"
	"github.com/javanhut/ollama_code/internal/semantic"
	"github.com/javanhut/ollama_code/internal/storage"
	"github.com/javanhut/ollama_code/tools"
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
	modalDiffAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Background(modalBg)  // + lines
	modalDiffDel     = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Background(modalBg) // - lines
)

// diffPreviewTools are the mutating tools whose permission-modal preview is a
// diff (+/- lines), so it gets colorized instead of shown as flat muted text.

const (
	minInputLines = 1
	maxInputLines = 20
)

// appVersion is stamped at build time via
// -ldflags "-X github.com/javanhut/ollama_code/tui.appVersion=$(VERSION)".
// It's a var (not const) so the linker can override the default.
var appVersion = "dev"

type state int

const (
	stateSettings state = iota
	stateModelPicker
	stateChat
	stateHelp
	statePermission
	stateNotes
	stateDiff
)

// settingsField identifies the focused input in the connection settings modal.
type settingsField int

const (
	settingsFocusURL settingsField = iota
	settingsFocusKey
)

type config struct {
	Host     string   `json:"host"`
	APIKey   string   `json:"api_key,omitempty"` // bearer token for Ollama Cloud (ollama.com) or an authenticated daemon
	Model    string   `json:"model,omitempty"`
	Activity []string `json:"activity,omitempty"`
	Verbose  bool     `json:"verbose,omitempty"`
	Thinking bool     `json:"show_thinking,omitempty"` // replay the reasoning stream in the transcript

	MaxSteps   int                     `json:"max_steps,omitempty"`   // tool-call budget per user turn (default 25)
	EmbedModel string                  `json:"embed_model,omitempty"` // model for auto-RAG embeddings
	AutoRAG    *bool                   `json:"auto_rag,omitempty"`    // nil/true = enabled
	Dream      *bool                   `json:"dream,omitempty"`       // nil/true = dream mode enabled
	Face       *bool                   `json:"face,omitempty"`        // nil/true = mascot overlay shown
	Welcome    *bool                   `json:"welcome,omitempty"`     // nil/true = show welcome panel on empty chat
	Verify     *bool                   `json:"verify,omitempty"`      // nil/true = auto compile-check on file edits
	VerifyCmd  string                  `json:"verify_cmd,omitempty"`  // override the auto-detected check
	Profiles   map[string]ModelProfile `json:"profiles,omitempty"`    // per-model, keyed by model name
}

// ModelProfile holds per-model settings discovered from /api/show (and cached)
// plus optional sampling overrides, so num_ctx and tool support adapt to the
// actual model instead of a hardcoded value.
type ModelProfile struct {
	NumCtx           int      `json:"num_ctx"`
	SupportsTools    bool     `json:"supports_tools"`
	SupportsThinking bool     `json:"supports_thinking,omitempty"`
	ParamsB          float64  `json:"params_b,omitempty"` // parameter count in billions; 0 = unknown
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	NumPredict       *int     `json:"num_predict,omitempty"`
}

// smallModelParamsB is the tier cutoff: models under this many billion
// parameters get the compact prompt, lean toolset, and low-temperature
// defaults. Unknown size (0) is treated as big — current behavior.
const smallModelParamsB = 15

func (p ModelProfile) smallModel() bool {
	return p.ParamsB > 0 && p.ParamsB < smallModelParamsB
}

func (m *Model) logActivity(s string) {
	m.cfg.Activity = append([]string{s}, m.cfg.Activity...)
	if len(m.cfg.Activity) > 5 {
		m.cfg.Activity = m.cfg.Activity[:5]
	}
	saveConfig(m.cfg)
}

// gen on the chat/tool messages is the turn generation they were produced
// under (see Model.turnGen). Handlers drop any message whose gen doesn't match
// the current generation — a straggler from a cancelled or replaced turn must
// not write into the new turn's state.

var (
	defaultToolCallTimeout   = 2 * time.Minute
	localInspectToolTimeout  = 30 * time.Second
	localMutatingToolTimeout = 90 * time.Second
	networkToolTimeout       = 2 * time.Minute
	longRunningToolTimeout   = 10 * time.Minute
	shellToolTimeoutGrace    = 5 * time.Second
	modelStreamIdleTimeout   = 3 * time.Minute
	pullIdleTimeout          = 5 * time.Minute
)

type Model struct {
	cfg           config
	host          api.OllamaHost
	tools         *tools.Registry
	notes         *sessionNotes
	todos         *todoList
	mode          Mode
	state         state
	urlInput      textinput.Model
	keyInput      textinput.Model
	settingsFocus settingsField
	models        []string
	picker        int
	modelName     string

	// Model pulling (from the model picker). pullInput captures the name to
	// pull; pullStream/progress fields drive the live download UI.
	pullInput     textinput.Model
	pulling       bool
	pullName      string
	pullStatus    string
	pullErr       string
	pullCompleted int64
	pullTotal     int64
	pullStream    *pullStreamState
	pullSelect    string // after a successful pull, land the picker cursor here
	profile       ModelProfile
	pending       *pendingBatch

	history    []api.Message
	transcript *strings.Builder
	viewport   viewport.Model
	input      textarea.Model
	stream     *streamState
	streaming  bool
	streamBuf  *strings.Builder
	thinkTail  string // rolling tail of the reasoning stream, shown as a ticker while thinking
	statusMsg  string
	statusErr  bool
	lastError  string
	toast      string
	sel        selection

	md      *markdownRenderer // chat transcript renderer (own width + cache)
	notesMd *markdownRenderer // notes-panel renderer (own width + cache)

	faceMoodCache faceMood // cached mood; recomputed only when history grows
	faceMoodLen   int      // len(history) the cached mood was computed at

	notesViewport viewport.Model
	diffViewport  viewport.Model // full-screen scrollable diff viewer (/diff)
	helpViewport  viewport.Model // scrollable help viewer (/help)
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
	retrieving     bool   // RAG retrieval is gating the model call for this turn

	// Loop safety (reset each user turn).
	turnGen             int            // bumped on every stream start and cancel; stale async msgs are dropped by gen mismatch
	streamRetries       int            // transient stream errors retried this turn
	stepCount           int            // tool-call rounds since the last user message
	autoContinues       int            // times we've nudged the model to keep going on open todos this turn
	maxSteps            int            // budget per turn (cfg.MaxSteps, default 25)
	recentCalls         []string       // ring of recent call fingerprints (oscillation)
	failedCalls         map[string]int // fingerprint -> consecutive failure count
	oscillationWarned   bool           // corrective nudge emitted once per turn
	suppressToolsOnce   bool           // next stream sends no tools (step budget hit)
	lastStepRepeatKey   string         // semantic identity of the previous single-tool batch
	sameToolStreak      int            // consecutive steps repeating that identity
	sameToolWarned      bool           // early repeat warning emitted this user turn
	sameToolStopWarned  bool           // hard-stop explanation emitted this user turn
	turnTouchedFiles    bool           // a file-mutating tool succeeded this turn
	verifying           bool           // a compile check is running
	verifyAttempts      int            // failed compile checks this turn
	challengedThisTurn  bool           // self-check challenge already issued this turn
	turnReads           map[string]int // read tool + cleaned path -> times read this turn
	rereadEvents        int            // re-reads of unchanged files this turn
	rereadStopAnnounced bool           // hard-stop explanation for re-read loops emitted
	lastPreamble        string         // normalized previous assistant preamble this turn
	preambleStreak      int            // consecutive near-duplicate preambles
	preambleWarned      bool           // preamble-echo warning emitted this turn

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
	faceFrame       int
	faceLastKey     time.Time

	// Per-turn record, keyed by the index of the user message that started the
	// turn, so each answer can report what it cost and what it was thinking.
	turnRecords   map[int]turnRecord
	turnAnchor    int
	turnStart     time.Time
	turnToolStart time.Time
	turnToolTime  time.Duration
	turnToolCalls int
	turnThinking  strings.Builder
}

// turnRecord is what one user command produced: wall clock end to end, the
// slice of it spent running tools (the rest is model time), and the reasoning
// stream, kept so /show_thinking can surface it after the fact.
type turnRecord struct {
	total    time.Duration
	tools    time.Duration
	calls    int
	thinking string
}

type liveEmbedder struct{ m *Model }

func (e liveEmbedder) Embed(model string, inputs []string) ([][]float32, error) {
	return e.m.host.Embed(model, inputs)
}

func New() *Model {
	cfg := loadConfig()
	// ... (host setup ...)
	host := api.OllamaHost{}
	host.SetURI(cfg.Host)
	host.SetAPIKey(resolveAPIKey(cfg))

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

	ki := textinput.New()
	ki.Prompt = "Key  "
	ki.Placeholder = "ollama.com api key (leave blank for local)"
	ki.SetValue(cfg.APIKey)
	ki.EchoMode = textinput.EchoPassword
	ki.EchoCharacter = '•'
	ki.SetWidth(60)

	pi := textinput.New()
	pi.Prompt = "Pull  "
	pi.Placeholder = "model to pull, e.g. qwen3-coder:30b or gpt-oss:120b-cloud"
	pi.SetWidth(60)

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
	registry := tools.DefaultRegistry()
	registry.Register(readNotesTool(notes))
	registry.Register(updateNotesTool(notes))
	registry.Register(appendNotesTool(notes))
	todos := &todoList{}
	registry.Register(todoWriteTool(todos))
	registry.Register(rememberTool(mem))
	registry.Register(recallTool(mem))
	registry.Register(forgetTool(mem))

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(accentColor)

	m := &Model{
		cfg:          cfg,
		host:         host,
		tools:        registry,
		notes:        notes,
		todos:        todos,
		mode:         ExploreMode,
		state:        stateChat,
		urlInput:     ti,
		keyInput:     ki,
		pullInput:    pi,
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
		md:           newMarkdownRenderer(),
		notesMd:      newMarkdownRenderer(),
		faceMoodLen:  -1, // force first mood computation
		expandTools:  false,
		lastActivity: time.Now(),
		faceLastKey:  time.Now(),
	}

	m.lastActivity = time.Now()
	registry.Register(m.switchModeTool())
	registry.Register(m.spawnSubagentTool())
	registry.Register(m.parallelEditTool())
	// Registered after m exists so the semantic tools read the live host and
	// pick up connection changes made via /settings.
	registry.Register(tools.CodeIndexTool(liveEmbedder{m}))
	registry.Register(tools.SemanticSearchTool(liveEmbedder{m}))
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

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, m.nextFaceTick()}
	// If no model is configured, try to load the first one we can find.
	if strings.TrimSpace(m.modelName) == "" {
		cmds = append(cmds, m.autoLoadModels())
	}
	return tea.Batch(cmds...)
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.urlInput.SetWidth(min(m.width-6, 80))
	m.keyInput.SetWidth(min(m.width-6, 80))
	m.pullInput.SetWidth(min(m.width-6, 80))

	// Size the textarea BEFORE measuring the band: inputView() renders the
	// textarea, so measuring first would size the viewport from the previous
	// frame's wrap and leave a stale row behind when the input shrinks.
	m.input.SetWidth(max(1, m.width-lipgloss.Width(m.inputPrefix())))

	headerH := lipgloss.Height(m.headerView())
	inputH := max(lipgloss.Height(m.inputView()), 2)
	vpH := max(m.height-headerH-inputH, 1)

	vpW := max(m.width-m.sidebarSpace(), 10)
	notesW := max(sidebarInner(m.sidebarWidth()), 1)
	notesVH := m.sidebarNotesHeight(vpH)

	// Diff viewer fills most of the screen (modal-width box, minus border/header).
	// Its box is border (2) + Padding(0,1) (2), so the text width is w-4.
	diffVW := max(m.modalWidth()-4, 20)
	diffVH := max(m.height-6, 4)

	// Help viewer: modal box minus border (2) + padding (4) horizontally, and
	// minus chrome + header/blank lines vertically.
	helpVW := max(m.modalWidth()-6, 20)
	helpVH := max(m.height-8, 4)

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
			viewport.WithWidth(notesW),
			viewport.WithHeight(notesVH),
		)
		m.diffViewport = viewport.New(
			viewport.WithWidth(diffVW),
			viewport.WithHeight(diffVH),
		)
		m.helpViewport = viewport.New(
			viewport.WithWidth(helpVW),
			viewport.WithHeight(helpVH),
		)
	} else {
		m.viewport.SetWidth(vpW)
		m.viewport.SetHeight(vpH)
		m.notesViewport.SetWidth(notesW)
		m.notesViewport.SetHeight(notesVH)
		m.diffViewport.SetWidth(diffVW)
		m.diffViewport.SetHeight(diffVH)
		m.helpViewport.SetWidth(helpVW)
		m.helpViewport.SetHeight(helpVH)
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

}

// atFirstVisualRow / atLastVisualRow report whether the cursor sits on the
// topmost / bottommost rendered row of the input, accounting for soft wrap: a
// single long hard line still occupies several rows, and up/down must walk
// those rows before they mean anything else.
func (m *Model) atFirstVisualRow() bool {
	return m.input.Line() == 0 && m.input.LineInfo().RowOffset == 0
}

func (m *Model) atLastVisualRow() bool {
	li := m.input.LineInfo()
	return m.input.Line() == m.input.LineCount()-1 && li.RowOffset >= li.Height-1
}

// inputIsHistory reports whether the buffer is safe to overwrite with a history
// entry — empty, or still exactly the entry we last recalled.
func (m *Model) inputIsHistory() bool {
	if m.input.Value() == "" {
		return true
	}
	return m.historyIndex < len(m.userHistory) && m.input.Value() == m.userHistory[m.historyIndex]
}
