package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// sidebarCols is the sidebar's total on-screen width, borders included.
const sidebarCols = 32

// sidebarGap is the blank column pair between the transcript and the sidebar.
const sidebarGap = "  "

// sidebarWidth returns the sidebar's total width, or 0 when the terminal is too
// narrow to spare the columns.
func (m *Model) sidebarWidth() int {
	if m.width < 60 {
		return 0
	}
	return sidebarCols
}

// sidebarSpace is how many columns the sidebar takes away from the transcript,
// gap included.
func (m *Model) sidebarSpace() int {
	if w := m.sidebarWidth(); w > 0 {
		return w + len(sidebarGap)
	}
	return 0
}

// sidebarInner is the text width inside the sidebar's border and padding.
func sidebarInner(w int) int { return w - 4 }

// statusText names the current phase of the turn, and whether that phase is
// still working (so the caller can prefix a spinner). The text is plain so it
// can be truncated without cutting an escape sequence in half.
func (m *Model) statusText() (string, bool) {
	switch {
	case m.pending != nil:
		if label := m.currentToolLabel(); label != "" {
			return fmt.Sprintf("TOOLS %d/%d · %s%s", m.pending.done, len(m.pending.calls), label, m.elapsedSuffix()), true
		}
		return fmt.Sprintf("TOOLS %d/%d%s", m.pending.done, len(m.pending.calls), m.elapsedSuffix()), true
	case m.retrieving:
		return "SEARCHING CODE" + m.elapsedSuffix(), true
	case m.compacting:
		return "COMPACTING" + m.elapsedSuffix(), true
	case m.streaming && m.streamBuf.Len() == 0:
		return "THINKING" + m.elapsedSuffix(), true
	case m.streaming:
		return "STREAMING" + m.elapsedSuffix(), true
	case m.verifying:
		return "VERIFYING" + m.elapsedSuffix(), true
	case m.dreaming:
		return "DREAMING", true
	case m.asleep:
		return "ASLEEP", false
	}
	return "READY", false
}

func (m *Model) sidebarHeading(s string) string {
	return headingStyle.Copy().Foreground(m.mode.color()).Render(s)
}

// sidebarSections returns the panel's stacked blocks — everything except the
// notes viewport and the key hints, which the caller positions itself.
func (m *Model) sidebarSections(inner int) []string {
	var out []string

	mode := lipgloss.NewStyle().Foreground(m.mode.color()).Bold(true).Render(strings.ToUpper(m.mode.String()))
	out = append(out, m.sidebarHeading("Mode")+"\n"+mode+"\n"+mutedStyle.Copy().Width(inner).Render(m.mode.hint()))

	text, busy := m.statusText()
	line := bodyStyle.Copy().Bold(true).Render(truncatePlain(text, inner-2))
	if busy {
		line = m.spinner.View() + " " + line
	}
	status := m.sidebarHeading("Status") + "\n" + line
	if m.totalTokens > 0 {
		tokens := fmt.Sprintf("%dk / %dk ctx", m.totalTokens/1000, m.contextLimit/1000)
		if m.totalTokens > m.contextLimit*8/10 {
			status += "\n" + errorStyle.Render(tokens)
		} else {
			status += "\n" + mutedStyle.Render(tokens)
		}
	}
	if m.toast != "" {
		status += "\n" + hintStyle.Copy().Width(inner).Render(m.toast)
	}
	out = append(out, status)

	if todo := m.todoSidebar(inner); todo != "" {
		out = append(out, todo)
	}
	if dreams := m.dreamSidebar(inner); dreams != "" {
		out = append(out, dreams)
	}
	return out
}

// dreamSidebar renders sleep state and the dreams gathered this session. Empty
// while awake with nothing dreamt.
func (m *Model) dreamSidebar(inner int) string {
	if !m.asleep && !m.dreaming && len(m.dreams) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.sidebarHeading(fmt.Sprintf("Dreams (%d)", len(m.dreams))))
	switch {
	case m.dreaming:
		b.WriteString("\n" + mutedStyle.Render("reflecting…"))
	case m.asleep:
		b.WriteString("\n" + mutedStyle.Render(fmt.Sprintf("asleep · %d pending", len(m.pendingDreams))))
	}
	start := max(len(m.dreams)-3, 0)
	for _, d := range m.dreams[start:] {
		b.WriteString("\n" + mutedStyle.Render("· "+truncatePlain(d.summary, inner-2)))
	}
	return b.String()
}

// sidebarKeys renders the pinned key legend as an aligned two-column list. It
// used to be two prose lines, and "shift+tab mode · ctrl+t tools" is one column
// wider than the panel — the terminal wrapped it and left "tools" orphaned.
func (m *Model) sidebarKeys() string {
	rows := [][2]string{
		{"shift+tab", "mode"},
		{"ctrl+t", "tools"},
		{"enter", "send"},
		{"/help", "commands"},
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(hintStyle.Render(padCell(r[0], 10)) + mutedStyle.Render(r[1]))
	}
	return b.String()
}

// sidebarView renders the always-on right panel to exactly `height` rows.
func (m *Model) sidebarView(height int) string {
	w := m.sidebarWidth()
	if w == 0 || height < 6 {
		return ""
	}
	inner := sidebarInner(w)
	body := strings.Join(m.sidebarSections(inner), "\n\n")
	if m.showNotes {
		body += "\n\n" + m.sidebarHeading("Notes") + "\n" + m.notesViewport.View()
	}
	// A rule above the pinned legend makes the empty space between the sections
	// and the bottom of the panel read as layout instead of a gap.
	keys := borderStyle.Render(strings.Repeat("─", inner)) + "\n" + m.sidebarKeys()

	// Width/Height count the border, so the rows we can fill are height-2. Fit
	// the content to that budget ourselves: overflowing it used to leave the box
	// unclosed at the bottom, because MaxHeight clipped the border row away.
	rows := fitSidebar(strings.Split(body, "\n"), strings.Split(keys, "\n"), height-2)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.mode.color()).
		Padding(0, 1).
		Width(w).
		Height(height).
		MaxHeight(height).
		Render(strings.Join(rows, "\n"))
}

// fitSidebar lays body and keys into exactly budget rows: keys pinned to the
// bottom with blank rows between, body clipped when it doesn't fit, and the
// keys themselves dropped when even that isn't enough.
func fitSidebar(body, keys []string, budget int) []string {
	if budget <= 0 {
		return nil
	}
	if len(body)+1+len(keys) > budget {
		if budget > len(keys)+1 {
			body = body[:budget-len(keys)-1]
		} else {
			return body[:min(len(body), budget)]
		}
	}
	out := body
	for len(out)+len(keys) < budget {
		out = append(out, "")
	}
	return append(out, keys...)
}

// sidebarNotesHeight is the room left for the notes viewport once the fixed
// blocks and key hints have taken theirs.
func (m *Model) sidebarNotesHeight(vpH int) int {
	w := m.sidebarWidth()
	if w == 0 {
		return 3
	}
	inner := sidebarInner(w)
	used := lipgloss.Height(strings.Join(m.sidebarSections(inner), "\n\n"))
	used += lipgloss.Height(m.sidebarKeys())
	used += 3 // blank separator, notes heading, bottom fill
	return max(vpH-2-used, 3)
}
