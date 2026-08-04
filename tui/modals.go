package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javanhut/ollama_code/api"
)

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
	w := min(min(max(m.width*3/5, 50), 78), m.width-4)
	return w
}

// modalInner is the text width inside a modalStyle box: the box width minus its
// border (2) and horizontal padding (4). Getting this wrong by even a column
// wraps every header, so the title and its "esc" hint land on separate lines.
func (m *Model) modalInner() int { return m.modalWidth() - 6 }

func (m *Model) modalHeader(title, hint string, innerW int) string {
	t := modalTitleStyle.Render(title)
	h := modalHintStyle.Render(hint)
	pad := max(1, innerW-lipgloss.Width(t)-lipgloss.Width(h))
	return t + modalBodyStyle.Render(strings.Repeat(" ", pad)) + h
}

func (m *Model) settingsModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	for _, in := range []*textinput.Model{&m.urlInput, &m.keyInput, &m.nameInput, &m.envInput} {
		in.SetWidth(innerW - 6)
	}

	targets := m.settingsTargets()
	target := m.settingsTargetName()
	label := "default host"
	switch {
	case m.settingsIsNew():
		label = "+ new provider"
	case target != "":
		label = target
	}

	var b strings.Builder
	b.WriteString(m.modalHeader("Endpoints", "esc", innerW))
	b.WriteString("\n\n")
	b.WriteString(modalMutedStyle.Render("↑↓ ") +
		modalAccentStyle.Render("‹ "+truncatePlain(label, max(innerW-18, 8))+" ›") +
		modalMutedStyle.Render(fmt.Sprintf("  %d/%d", m.settingsTarget+1, len(targets))))
	b.WriteString("\n\n")

	for _, f := range m.settingsFields() {
		switch f {
		case settingsFocusName:
			b.WriteString(m.nameInput.View() + "\n")
		case settingsFocusURL:
			b.WriteString(m.urlInput.View() + "\n")
		case settingsFocusKey:
			b.WriteString(m.keyInput.View() + "\n")
		case settingsFocusEnv:
			b.WriteString(m.envInput.View() + "\n")
		case settingsFocusNative:
			wire := providerKindLabel(m.settingsKind)
			row := modalMutedStyle.Render("Wire  ") + modalBodyStyle.Render("‹ "+wire+" ›")
			if m.settingsFocus == settingsFocusNative {
				row = modalMutedStyle.Render("Wire  ") + modalAccentStyle.Render("‹ "+wire+" › ") +
					modalMutedStyle.Render("space")
			}
			b.WriteString(row + "\n")

		case settingsFocusTrust:
			state := "off — headless runs abort on Cursor's trust prompt"
			if m.settingsTrust {
				state = "on — passes --trust"
			}
			row := modalMutedStyle.Render("Trust ") + modalBodyStyle.Render("‹ "+state+" ›")
			if m.settingsFocus == settingsFocusTrust {
				row = modalMutedStyle.Render("Trust ") + modalAccentStyle.Render("‹ "+state+" › ") +
					modalMutedStyle.Render("space")
			}
			b.WriteString(row + "\n")
		}
	}

	b.WriteString(modalMutedStyle.Render(truncatePlain(m.settingsFieldHint(target), innerW)))
	b.WriteString("\n\n")
	if m.statusMsg != "" {
		style := modalMutedStyle
		if m.statusErr {
			style = modalErrorStyle
		}
		b.WriteString(style.Render(truncatePlain(m.statusMsg, innerW)))
		b.WriteString("\n\n")
	}

	hint := modalMutedStyle.Render("tab ") + modalBodyStyle.Render("field") +
		modalMutedStyle.Render("   ↑↓ ") + modalBodyStyle.Render("endpoint") +
		modalMutedStyle.Render("   enter ") + modalBodyStyle.Render("save & test")
	if target != "" {
		hint += modalMutedStyle.Render("   ctrl+d ") + modalBodyStyle.Render("delete")
	}
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
}

// settingsFieldHint explains the fields for the selected wire format. A cursor
// provider has no URL and needs no key when the CLI is already signed in, so the
// generic key advice would be actively misleading there.
func (m *Model) settingsFieldHint(target string) string {
	if m.settingsKind == api.ProviderCursor {
		if !m.settingsTrust {
			return "Trust off: runs here will fail until you accept Cursor's trust prompt yourself"
		}
		if strings.TrimSpace(os.Getenv("CURSOR_API_KEY")) != "" {
			return "URL is the agent binary path (blank = find it on PATH) · CURSOR_API_KEY is set"
		}
		return "URL is the agent binary path (blank = find it on PATH) · key optional if you already logged in"
	}
	return m.settingsKeyHint(target)
}

// settingsKeyHint explains what the key field actually controls for the selected
// endpoint. An environment variable silently outranks the stored key, so a user
// typing into a field that is being overridden has to be told.
func (m *Model) settingsKeyHint(target string) string {
	if target == "" {
		if strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")) != "" {
			return "OLLAMA_API_KEY is set — it overrides this field"
		}
		return "blank for local · set for ollama.com cloud models"
	}
	p := m.cfg.Providers[target]
	switch {
	case p.APIKeyEnv == "":
		return "key is stored in config.json — naming an env var above keeps it out"
	case strings.TrimSpace(os.Getenv(p.APIKeyEnv)) != "":
		return "$" + p.APIKeyEnv + " is set — it overrides this field"
	default:
		return "$" + p.APIKeyEnv + " is unset — this field is the fallback"
	}
}

func (m *Model) pickerModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	var b strings.Builder
	b.WriteString(m.modalHeader("Select model", "esc", innerW))
	b.WriteString("\n\n")

	host := strings.TrimPrefix(strings.TrimPrefix(m.cfg.Host, "http://"), "https://")
	b.WriteString(modalMutedStyle.Render(truncatePlain(fmt.Sprintf("on %s", host), innerW)))
	b.WriteString("\n\n")

	// Pull-in-progress view takes over the modal body.
	if m.pulling {
		b.WriteString(modalAccentStyle.Render(truncatePlain("Pulling "+m.pullName, innerW)))
		b.WriteString("\n\n")
		b.WriteString(modalMutedStyle.Render(truncatePlain(m.pullStatus, innerW)))
		b.WriteString("\n")
		if m.pullTotal > 0 {
			b.WriteString(renderProgressBar(m.pullCompleted, m.pullTotal, innerW-2))
			b.WriteString("\n")
			b.WriteString(modalMutedStyle.Render(fmt.Sprintf("%s / %s", humanBytes(m.pullCompleted), humanBytes(m.pullTotal))))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(modalMutedStyle.Render("esc ") + modalBodyStyle.Render("cancel"))
		return modalStyle.Width(w).Render(b.String())
	}

	if m.pullErr != "" {
		b.WriteString(modalErrorStyle.Render(truncatePlain("pull failed: "+m.pullErr, innerW)))
		b.WriteString("\n")
		if hint := pullErrorHint(m.cfg.Host, m.pullErr); hint != "" {
			b.WriteString(modalMutedStyle.Render(ansi.Wordwrap(hint, innerW, " -")))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(m.models) == 0 {
		b.WriteString(modalMutedStyle.Render(truncatePlain("no models installed — press p to pull one", innerW)))
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

	// Name-entry view for pulling a new model.
	if m.pullInput.Focused() {
		m.pullInput.SetWidth(innerW - 8)
		b.WriteString("\n")
		b.WriteString(modalAccentStyle.Render("Pull a model"))
		b.WriteString("\n")
		b.WriteString(m.pullInput.View())
		b.WriteString("\n")
		guide := "Use the exact tag from ollama.com — cloud models need the -cloud suffix (e.g. gpt-oss:120b-cloud)."
		if strings.Contains(strings.ToLower(m.cfg.Host), "ollama.com") {
			guide = "Heads up: pulling needs your local daemon (http://localhost:11434), not ollama.com. Cloud models register through the daemon after `ollama signin`."
		}
		b.WriteString(modalMutedStyle.Render(ansi.Wordwrap(guide, innerW, " -")))
		b.WriteString("\n\n")
		hint := modalMutedStyle.Render("enter ") + modalBodyStyle.Render("pull") +
			modalMutedStyle.Render("   esc ") + modalBodyStyle.Render("cancel")
		b.WriteString(hint)
		return modalStyle.Width(w).Render(b.String())
	}

	b.WriteString("\n")
	hint := modalMutedStyle.Render("↑↓ ") + modalBodyStyle.Render("select") +
		modalMutedStyle.Render("   enter ") + modalBodyStyle.Render("chat") +
		modalMutedStyle.Render("   p ") + modalBodyStyle.Render("pull") +
		modalMutedStyle.Render("   r ") + modalBodyStyle.Render("refresh")
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
}

// pullErrorHint maps the common /api/pull failure modes to actionable guidance,
// since the daemon's raw error ("file does not exist", "unauthorized") rarely
// tells the user what to actually do. Returns "" when nothing useful applies.
func pullErrorHint(host, errMsg string) string {
	e := strings.ToLower(errMsg)
	onCloudHost := strings.Contains(strings.ToLower(host), "ollama.com")
	switch {
	case strings.Contains(e, "unauthorized") || strings.Contains(e, "401"):
		return "Cloud models are pulled through your LOCAL daemon (URL http://localhost:11434) after `ollama signin` — not directly against ollama.com, which only serves chat."
	case strings.Contains(e, "manifest"), strings.Contains(e, "does not exist"), strings.Contains(e, "not found"):
		if onCloudHost {
			return "You can't pull from ollama.com — point the URL at your local daemon (http://localhost:11434) to register a cloud model, then chat."
		}
		return "Check the exact tag — cloud models need the full size and the -cloud suffix (e.g. gpt-oss:120b-cloud). Copy the name from the model's page on ollama.com."
	default:
		return ""
	}
}

// humanBytes formats a byte count as a compact human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderProgressBar draws a [#####-----] bar of the given total width.
func renderProgressBar(completed, total int64, width int) string {
	if width < 4 {
		width = 4
	}
	inner := width - 2
	frac := 0.0
	if total > 0 {
		frac = float64(completed) / float64(total)
	}
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(inner))
	bar := "[" + strings.Repeat("#", filled) + strings.Repeat("-", inner-filled) + "]"
	return modalAccentStyle.Render(bar) + modalMutedStyle.Render(fmt.Sprintf(" %3.0f%%", frac*100))
}

func (m *Model) helpModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	header := m.modalHeader("Help", "esc", innerW)
	scroll := ""
	if !(m.helpViewport.AtTop() && m.helpViewport.AtBottom()) {
		scroll = fmt.Sprintf(" · %d%%", int(m.helpViewport.ScrollPercent()*100))
	}
	hint := modalMutedStyle.Render("↑/↓ · pgup/pgdn scroll · esc close" + scroll)
	return modalStyle.Width(w).Render(header + "\n" + hint + "\n\n" + m.helpViewport.View())
}

// helpContent renders the help rows into the given width. Kept separate from
// helpModal so the /help entry point can load it into the scroll viewport.
type helpRow struct{ key, desc string }

func (m *Model) helpContent(innerW int) string {
	rows := []helpRow{
		{"", "Modes"},
		{"explore", "read-only — model can only inspect"},
		{"plan", "read + update session notes"},
		{"write", "all tools; writes need your approval"},
		{"auto", "autonomous — unlimited changes in workspace"},
		{"shift+tab", "cycle modes"},
		{"", ""},
		{"", "Slash commands"},
	}
	// Generated from the completion menu's list so the two can't drift — the help
	// screen used to hardcode half of them.
	for _, c := range slashCommands {
		rows = append(rows, helpRow{c.name, c.desc})
	}
	rows = append(rows, []helpRow{
		{"", ""},
		{"", "Keys"},
		{"enter", "send message"},
		{"shift+enter", "newline in input"},
		{"shift+↑/↓", "scroll one line"},
		{"ctrl+↑/↓", "scroll one line (alt)"},
		{"pgup/pgdn", "page up/down"},
		{"ctrl+u/d", "half page up/down"},
		{"ctrl+g", "jump to the newest output"},
		{"ctrl+f", "find in the transcript (n/N to step)"},
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
	}...)

	var b strings.Builder
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
	return b.String()
}

func (m *Model) notesModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	var b strings.Builder
	b.WriteString(m.modalHeader("Session notes", "esc", innerW))
	b.WriteString("\n\n")
	notes := m.notes.get()
	if notes == "" {
		b.WriteString(modalMutedStyle.Render("(empty)"))
		b.WriteString("\n")
		b.WriteString(modalMutedStyle.Render("The model can read/append/replace these via tools."))
	} else {
		for line := range strings.SplitSeq(notes, "\n") {
			b.WriteString(modalBodyStyle.Render(truncatePlain(line, innerW)))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(modalMutedStyle.Render("esc ") + modalBodyStyle.Render("close"))
	return modalStyle.Width(w).Render(b.String())
}

// statsModal reports what the session has cost so far, built from the per-turn
// records the transcript footers already use.
func (m *Model) statsModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	var b strings.Builder
	b.WriteString(m.modalHeader("Session stats", "esc", innerW))
	b.WriteString("\n\n")

	row := func(k, v string) {
		b.WriteString(modalMutedStyle.Render(padCell(k, 16)) + modalBodyStyle.Render(v))
		b.WriteString("\n")
	}

	var total, tools, slowest time.Duration
	var calls, turns int
	for _, r := range m.turnRecords {
		turns++
		total += r.total
		tools += r.tools
		calls += r.calls
		slowest = max(slowest, r.total)
	}
	if turns == 0 {
		b.WriteString(modalMutedStyle.Render("No completed turns yet."))
		b.WriteString("\n\n")
		b.WriteString(modalMutedStyle.Render("esc ") + modalBodyStyle.Render("close"))
		return modalStyle.Width(w).Render(b.String())
	}

	row("turns", fmt.Sprintf("%d", turns))
	row("total time", shortDuration(total))
	row("thinking", shortDuration(total-tools))
	row("tools", fmt.Sprintf("%s · %d call%s", shortDuration(tools), calls, plural(calls)))
	row("slowest turn", shortDuration(slowest))
	row("average turn", shortDuration(total/time.Duration(turns)))
	b.WriteString("\n")
	if m.contextLimit > 0 {
		row("context", fmt.Sprintf("%dk / %dk tokens", m.totalTokens/1000, m.contextLimit/1000))
	}
	row("model", m.modelName)
	row("mode", strings.ToUpper(m.mode.String()))

	b.WriteString("\n")
	b.WriteString(modalMutedStyle.Render("esc ") + modalBodyStyle.Render("close"))
	return modalStyle.Width(w).Render(b.String())
}

// diffModal renders the full-screen, scrollable diff viewer (/diff). It uses a
// bordered box with no filled background so the diff colors from colorizeDiff
// read the same as they do in the transcript.
func (m *Model) diffModal() string {
	c := m.mode.color()
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c).
		Padding(0, 1).
		Width(m.modalWidth())
	title := headingStyle.Foreground(c).Render("Diff — last turn")
	hint := mutedStyle.Render("  ↑/↓ scroll · esc close")
	return style.Render(title + hint + "\n\n" + m.diffViewport.View())
}

// styleDiffLine colorizes one diff preview line on the modal background:
// additions green, deletions red, everything else muted.
func styleDiffLine(line string, w int) string {
	line = truncatePlain(line, w)
	switch {
	case strings.HasPrefix(line, "+"):
		return modalDiffAdd.Render(line)
	case strings.HasPrefix(line, "-"):
		return modalDiffDel.Render(line)
	default:
		return modalMutedStyle.Render(line)
	}
}

// routeConfirmModal offers the plan-mode model for a request that scored as
// planning work. Declining is the default: it costs nothing and stays local.
func (m *Model) routeConfirmModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()
	target := m.modelForMode(PlanMode)

	var lines []string
	lines = append(lines,
		m.modalHeader("This looks like planning work", "n=stay local", innerW),
		"",
		modalBodyStyle.Render("Run it in plan mode on")+" "+modalAccentStyle.Render(target)+modalBodyStyle.Render("?"),
		modalMutedStyle.Render("staying local keeps "+m.modelName+" in "+m.mode.String()+" mode"),
	)
	if len(m.routeReasons) > 0 {
		lines = append(lines, "",
			modalMutedStyle.Render("matched: "+truncatePlain(strings.Join(m.routeReasons, ", "), innerW-9)))
	}
	lines = append(lines, "",
		modalMutedStyle.Render("y/enter ")+modalBodyStyle.Render("switch to plan   ")+
			modalMutedStyle.Render("n/esc ")+modalBodyStyle.Render("stay local"))

	return modalStyle.Width(w).Render(strings.Join(lines, "\n"))
}

func (m *Model) permissionModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()

	// Content must fit within m.height-4 (border 2 + vertical padding 2). This
	// used to floor at 8 lines, which on a short terminal deliberately overflowed
	// the screen — on the one modal you have to read before approving anything.
	maxLines := max(m.height-4, 1)

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
	availableLines := max(maxLines-usedLines,
		// absolute minimum for args/preview
		2)

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
		isDiff := diffPreviewTools[call.Function.Name]
		for i, line := range previewLines {
			if len(middleSection) >= availableLines-1 && i < len(previewLines)-1 {
				middleSection = append(middleSection, modalMutedStyle.Render(fmt.Sprintf("... (%d more lines of preview)", len(previewLines)-i)))
				break
			}
			if isDiff {
				middleSection = append(middleSection, styleDiffLine(line, innerW))
			} else {
				middleSection = append(middleSection, modalMutedStyle.Render(truncatePlain(line, innerW)))
			}
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

func pickerWindow(total, cursor, size int) windowRange {
	if total <= size {
		return windowRange{0, total}
	}
	start := max(cursor-size/2, 0)
	end := start + size
	if end > total {
		end = total
		start = end - size
	}
	return windowRange{start, end}
}
