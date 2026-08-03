package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
	m.urlInput.SetWidth(innerW - 6)
	m.keyInput.SetWidth(innerW - 6)
	var b strings.Builder
	b.WriteString(m.modalHeader("Connection", "esc", innerW))
	b.WriteString("\n\n")
	b.WriteString(modalMutedStyle.Render("URL"))
	b.WriteString("\n")
	b.WriteString(m.urlInput.View())
	b.WriteString("\n\n")
	b.WriteString(modalMutedStyle.Render("API key"))
	b.WriteString("\n")
	b.WriteString(m.keyInput.View())
	b.WriteString("\n")
	if strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")) != "" {
		b.WriteString(modalMutedStyle.Render(truncatePlain("using OLLAMA_API_KEY from environment", innerW)))
	} else {
		b.WriteString(modalMutedStyle.Render(truncatePlain("blank for local · set for ollama.com cloud models", innerW)))
	}
	b.WriteString("\n\n")
	if m.statusMsg != "" {
		if m.statusErr {
			b.WriteString(modalErrorStyle.Render(truncatePlain(m.statusMsg, innerW)))
		} else {
			b.WriteString(modalMutedStyle.Render(truncatePlain(m.statusMsg, innerW)))
		}
		b.WriteString("\n\n")
	}
	hint := modalMutedStyle.Render("tab ") + modalBodyStyle.Render("switch") +
		modalMutedStyle.Render("   enter ") + modalBodyStyle.Render("connect") +
		modalMutedStyle.Render("   esc ") + modalBodyStyle.Render("cancel")
	b.WriteString(hint)
	return modalStyle.Width(w).Render(b.String())
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
	title := headingStyle.Copy().Foreground(c).Render("Diff — last turn")
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

func (m *Model) permissionModal() string {
	w := m.modalWidth()
	innerW := m.modalInner()

	// We want the total lines of the modal content to fit within m.height - 4 (to account for borders & padding)
	maxLines := max(m.height-4,
		// absolute minimum safety boundary
		8)

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
