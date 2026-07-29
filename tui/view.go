package tui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/lipgloss/v2"
)

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
	{"/notes", "toggle session notes in the sidebar"},
	{"/diff", "view last turn's file diffs"},
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
	{"/face", "toggle the mascot overlay on/off"},
	{"/welcome", "toggle the startup welcome panel on/off"},
	{"/verify", "toggle auto compile-check after edits"},
	{"/verbose", "toggle detailed tool output"},
	{"/auto", "switch to autonomous mode"},
	{"/mode", "switch mode (explore, plan, write, auto)"},
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
			dup := slices.Contains(matches, c.name)
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

// liveEmbedder satisfies tools.Embedder by forwarding to the model's current
// host instead of a snapshot, so the semantic tools (code_index /
// semantic_search) honor connection changes — URL or API key — made via
// /settings mid-session.

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
	// Paint the animated mascot on top of the composed view, pinned bottom-right.
	base = m.overlayFace(base)
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
	case stateDiff:
		v.SetContent(m.overlayModal(base, m.diffModal()))
	default:
		v.SetContent(base)
	}
	return v
}

type windowRange struct{ start, end int }

func (m *Model) viewChat() string {
	main := m.viewport.View()
	if bar := m.sidebarView(m.viewport.Height()); bar != "" {
		main = lipgloss.JoinHorizontal(lipgloss.Top, main, sidebarGap, bar)
	}
	return fmt.Sprintf(
		"%s\n%s\n%s",
		m.headerView(),
		main,
		m.inputView(),
	)
}

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

	right := ""
	metaSpace := width - lipgloss.Width(brand) - lipgloss.Width(model) - 3
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

func slashDesc(name string) string {
	for _, c := range slashCommands {
		if c.name == name {
			return c.desc
		}
	}
	return ""
}

func (m *Model) slashSuggestionsView() string {
	if !m.slashVisible || len(m.slashSuggestions) == 0 {
		return ""
	}
	rowStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(lipgloss.Color("252"))
	selStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("39")).
		Foreground(lipgloss.Color("232")).
		Bold(true)
	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("245")).
		Italic(true)

	// Width the rows to the widest command + description so the highlight bar is
	// a clean vertical block.
	nameW, descW := 0, 0
	for _, s := range m.slashSuggestions {
		if w := lipgloss.Width(s); w > nameW {
			nameW = w
		}
		if w := lipgloss.Width(slashDesc(s)); w > descW {
			descW = w
		}
	}
	lineW := nameW + descW + 4

	var b strings.Builder
	for i, s := range m.slashSuggestions {
		text := fmt.Sprintf(" %-*s  %s", nameW, s, slashDesc(s))
		st := rowStyle
		if i == m.slashSelected {
			st = selStyle
		}
		b.WriteString(st.Width(lineW).Render(text))
		b.WriteString("\n")
	}
	b.WriteString(hintStyle.Render(" tab/shift+tab to cycle · enter to select "))
	return b.String()
}

// inputPrefix renders the label that sits left of the input band. layout()
// measures it to size the textarea, so the math lives in exactly one place.
// The label is pinned to a fixed width. It used to swap "message" for the much
// longer "queued while streaming", which changed the prefix width mid-typing and
// re-wrapped whatever was already in the buffer.
const inputLabelWidth = 7

func (m *Model) inputPrefix() string {
	c := m.mode.color()
	label := "message"
	if m.streaming {
		label = "queued"
	}
	return lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Padding(0, 1).
		Width(inputLabelWidth + 2).
		Render(label)
}

// inputPrefixColumn stacks the label over a matching gutter so it lines up with
// a textarea that has grown past one row. Plain concatenation left rows 2..N
// starting at column 0 while row 1 sat behind the label.
func (m *Model) inputPrefixColumn(height int) string {
	prefix := m.inputPrefix()
	if height <= 1 {
		return prefix
	}
	gutter := lipgloss.NewStyle().
		Background(panelColor).
		Render(strings.Repeat(" ", lipgloss.Width(prefix)))
	rows := make([]string, height)
	rows[0] = prefix
	for i := 1; i < height; i++ {
		rows[i] = gutter
	}
	return strings.Join(rows, "\n")
}

// narrowStatusLine is the one-line status strip shown above the input band
// when the terminal is too narrow for the sidebar (<60 cols); wider layouts
// carry status and toast in the sidebar instead. It is always exactly one
// line so the band height — and the layout math built on it — stays stable.
func (m *Model) narrowStatusLine() string {
	if m.sidebarWidth() > 0 {
		return ""
	}
	text, busy := m.statusText()
	var line string
	if busy {
		line = m.spinner.View() + " " + bodyStyle.Copy().Bold(true).Render(text)
	} else {
		line = mutedStyle.Render(text)
	}
	if m.toast != "" {
		line += "  " + hintStyle.Render(truncatePlain(m.toast, max(m.width-lipgloss.Width(text)-4, 10)))
	}
	return line
}

func (m *Model) inputView() string {
	// The mascot is drawn separately as a corner overlay (see overlayFace), so
	// the input takes the full width here. The textarea already wraps at the
	// width layout() gave it (same prefix math), so the band must NOT re-wrap
	// with Style.Width — that hard re-wrap mangled long pastes into orphan
	// fragments.
	input := inputBandStyle.Render(m.input.View())
	bottomBar := lipgloss.JoinHorizontal(
		lipgloss.Top,
		m.inputPrefixColumn(lipgloss.Height(input)),
		input,
	)

	suggestions := m.slashSuggestionsView()
	if status := m.narrowStatusLine(); status != "" {
		return suggestions + "\n" + status + "\n" + bottomBar
	}
	return suggestions + "\n" + bottomBar
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
