package tui

import (
	"fmt"
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
	{"/settings", "edit endpoint URLs and API keys"},
	{"/model", "show/set current model (use, ctx, temp)"},
	{"/models", "list, switch, or pull models"},
	{"/route", "bind a model to a mode (big for plan, small for write)"},
	{"/provider", "add/edit an OpenAI-compatible endpoint and its key"},
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
	{"/stats", "session timing and token totals"},
	{"/show_thinking", "toggle the model's reasoning in the transcript"},
	{"/auto", "switch to autonomous mode"},
	{"/mode", "switch mode (explore, plan, write, auto)"},
}

// isSlashCommand reports whether val is exactly a known command. The suggestion
// list deliberately omits the exact match, so without this check Enter would
// swap a fully-typed "/model" for the only remaining suggestion, "/models".
func isSlashCommand(val string) bool {
	for _, c := range slashCommands {
		if c.name == val {
			return true
		}
	}
	return false
}

func (m *Model) dismissSlash() {
	m.slashVisible = false
	m.slashSuggestions = nil
	m.slashSelected = 0
}

func (m *Model) updateSlashSuggestions() {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") || strings.Contains(val, " ") || strings.Contains(val, "\n") {
		m.dismissSlash()
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
		m.dismissSlash()
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
	case stateRouteConfirm:
		v.SetContent(m.overlayModal(base, m.routeConfirmModal()))
	case stateDiff:
		v.SetContent(m.overlayModal(base, m.diffModal()))
	case stateStats:
		v.SetContent(m.overlayModal(base, m.statsModal()))
	default:
		v.SetContent(base)
	}
	return v
}

type windowRange struct{ start, end int }

func (m *Model) viewChat() string {
	main := m.viewport.View()
	// Paint the scroll cue over the transcript's last row rather than adding a
	// band: a band changes the viewport height, which changes whether the user
	// is at the bottom, which decides whether the cue shows at all.
	if cue := m.scrollCue(); cue != "" {
		main = overlay(main, cue, max(m.viewport.Width()-lipgloss.Width(cue), 0), m.viewport.Height()-1)
	}
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

	chip := lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("232")).
		Bold(true).
		Padding(0, 1)
	mode := strings.ToUpper(m.mode.String())
	brand := chip.Render("ollama code")
	modeChip := chip.Render(mode)
	// Shrink the badges before anything is allowed to wrap onto a second row.
	if lipgloss.Width(brand)+lipgloss.Width(modeChip) > width {
		brand = chip.Render("oc")
	}
	if lipgloss.Width(brand)+lipgloss.Width(modeChip) > width {
		modeChip = chip.Render(mode[:1])
	}
	if lipgloss.Width(brand)+lipgloss.Width(modeChip) > width {
		modeChip = ""
	}

	// Right edge: branch and context usage, then the mode chip — it balances the
	// brand badge and keeps the mode visible when the sidebar is hidden. The
	// whole right half of this row used to be dead space.
	var meta []string
	if m.gitBranch != "" {
		// Trim the branch, not the joined string — a long branch name would
		// otherwise eat the context counter, which is the more useful half.
		meta = append(meta, truncatePlain(m.gitBranch, max(width/5, 12)))
	}
	if m.totalTokens > 0 && m.contextLimit > 0 {
		meta = append(meta, fmt.Sprintf("%dk/%dk ctx", m.totalTokens/1000, m.contextLimit/1000))
	}
	metaStyle := mutedStyle.Background(surfaceColor)
	if m.contextLimit > 0 && m.totalTokens > m.contextLimit*8/10 {
		metaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Background(surfaceColor)
	}
	// roomFor is what the left side has left once brand, right side and the gap
	// between them are accounted for. Both the meta and the model name test
	// against it, so they can't disagree about who gets the last columns.
	who := m.activeModelName()
	roomFor := func(right string) int {
		return width - lipgloss.Width(brand) - lipgloss.Width(right) - 4
	}
	right := modeChip
	if s := strings.Join(meta, " · "); s != "" && width >= 60 {
		// Identity outranks the meta: only keep branch/ctx if the name still fits.
		if withMeta := metaStyle.Render(s+"  ") + right; roomFor(withMeta) > lipgloss.Width(who) {
			right = withMeta
		}
	}

	// Left: assistant name plus the loaded model, trimmed to what's left over.
	left := brand
	if room := roomFor(right); room > lipgloss.Width(who) {
		label := bodyStyle.Background(surfaceColor).Bold(true).Render(who)
		if name := strings.TrimSpace(m.modelName); name != "" {
			// Below ~10 columns a truncated model name is noise; drop it instead.
			if avail := room - lipgloss.Width(who) - 3; avail >= 10 {
				label += mutedStyle.Background(surfaceColor).Render(" · " + truncatePlain(name, avail))
			}
		}
		left += chromeStyle.Render("  ") + label
	}

	pad := max(0, width-lipgloss.Width(left)-lipgloss.Width(right))
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

// slashMenuRows caps how many commands the completion menu shows at once. A
// bare "/" matches every command, and drawing all of them buried the transcript
// and squeezed the sidebar until its box couldn't close.
const slashMenuRows = 8

func (m *Model) slashSuggestionsView() string {
	if !m.slashVisible || len(m.slashSuggestions) == 0 {
		return ""
	}
	c := m.mode.color()
	nameStyle := lipgloss.NewStyle().Foreground(c).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selStyle := lipgloss.NewStyle().Background(c).Foreground(lipgloss.Color("232")).Bold(true)

	total := len(m.slashSuggestions)
	rows := slashMenuRows
	if m.height > 0 {
		rows = clamp(m.height/3, 3, slashMenuRows) // never eat the transcript on a short terminal
	}
	win := pickerWindow(total, m.slashSelected, rows)

	hint := "↑↓ move · tab complete · enter select"
	counter := ""
	if win.end-win.start < total {
		counter = fmt.Sprintf("%d/%d", m.slashSelected+1, total)
	}

	// Measure across every match, not just the visible window, so the box
	// doesn't jitter in width while scrolling.
	nameW, descW := 0, 0
	for _, s := range m.slashSuggestions {
		nameW = max(nameW, lipgloss.Width(s))
		descW = max(descW, lipgloss.Width(slashDesc(s)))
	}
	inner := max(nameW+descW+4, lipgloss.Width(hint)+lipgloss.Width(counter)+2)
	if m.width > 0 {
		inner = min(inner, m.width-5) // border (2) + padding (2) + left margin (1)
	}
	descCol := max(inner-nameW-4, 1)

	var lines []string
	for i := win.start; i < win.end; i++ {
		s := m.slashSuggestions[i]
		desc := slashDesc(s)
		if i == m.slashSelected {
			text := fmt.Sprintf("› %-*s  %s", nameW, s, desc)
			lines = append(lines, selStyle.Width(inner).Render(truncatePlain(text, inner)))
			continue
		}
		lines = append(lines,
			"  "+nameStyle.Render(fmt.Sprintf("%-*s", nameW, s))+"  "+descStyle.Render(truncatePlain(desc, descCol)))
	}

	// A rule and a caption row close the box: hint left, position right.
	lines = append(lines, borderStyle.Render(strings.Repeat("─", inner)))
	gap := max(inner-lipgloss.Width(hint)-lipgloss.Width(counter), 1)
	lines = append(lines, hintStyle.Render(hint)+strings.Repeat(" ", gap)+mutedStyle.Render(counter))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c).
		Padding(0, 1).
		MarginLeft(1).
		Width(inner + 4).
		Render(strings.Join(lines, "\n"))
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
	// Width, not sidebarWidth(): layout() measures this band before it knows the
	// viewport height, and sidebar visibility depends on that height.
	if m.width >= 60 {
		return ""
	}
	text, busy := m.statusText()
	var line string
	if busy {
		line = m.spinner.View() + " " + bodyStyle.Bold(true).Render(text)
	} else {
		line = mutedStyle.Render(text)
	}
	if toast := m.activeToast(); toast != "" {
		line += "  " + hintStyle.Render(truncatePlain(toast, max(m.width-lipgloss.Width(text)-4, 10)))
	}
	return line
}

// scrollCue tells the user the transcript continues below when they've scrolled
// up — otherwise a streaming reply lands off-screen with nothing to say so.
// Only shown when already scrolled up, so adding the row can't flip the state
// that produced it.
func (m *Model) scrollCue() string {
	if m.viewport.Height() <= 0 || m.viewport.AtBottom() {
		return ""
	}
	below := m.viewport.TotalLineCount() - m.viewport.YOffset() - m.viewport.Height()
	if below <= 0 {
		return ""
	}
	text := fmt.Sprintf(" ↓ %d more line%s · ctrl+g to jump ", below, plural(below))
	if w := m.viewport.Width(); lipgloss.Width(text) > w {
		text = fmt.Sprintf(" ↓ %d ", below)
		if lipgloss.Width(text) > w {
			return ""
		}
	}
	return lipgloss.NewStyle().
		Background(surfaceColor).
		Foreground(lipgloss.Color("245")).
		Render(text)
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

	// Join only the bands that have content — an empty suggestion menu used to
	// contribute a blank row, leaving a dead gap above the input.
	var bands []string
	if s := m.searchBar(); s != "" {
		bands = append(bands, s)
	}
	if s := m.slashSuggestionsView(); s != "" {
		bands = append(bands, s)
	}
	if status := m.narrowStatusLine(); status != "" {
		bands = append(bands, status)
	}
	return strings.Join(append(bands, bottomBar), "\n")
}

// emptyState is the quiet stand-in for the welcome panel when it's switched
// off: enough to orient a fresh session without filling the screen.
func (m *Model) emptyState() string {
	rows := []string{
		"",
		mutedStyle.Render("Ask for a change, or start with a command."),
		"",
		hintStyle.Render("/help      ") + mutedStyle.Render("all commands"),
		hintStyle.Render("shift+tab  ") + mutedStyle.Render("switch mode"),
		hintStyle.Render("@file      ") + mutedStyle.Render("pull a file into the message"),
	}
	return "  " + strings.Join(rows, "\n  ") + "\n"
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
	panelBorder := borderStyle.Foreground(m.mode.color())
	top := panelBorder.Render("╭─") + headingStyle.Render(title) + panelBorder.Render(strings.Repeat("─", topFill)+"╮")
	bottom := panelBorder.Render("╰" + strings.Repeat("─", width-2) + "╯")
	inner := width - 4 // 1 char border + 1 char pad on each side
	rowStyle := lipgloss.NewStyle().Background(surfaceColor)

	rows := []string{""}
	rows = append(rows, centerCell(bodyStyle.Bold(true).Render("Layla's in. Let's write something worth keeping."), inner))
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

func (m *Model) activeModelName() string {
	if strings.TrimSpace(m.modelName) == "" {
		return "Layla (no brain)"
	}
	return "Layla"
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
