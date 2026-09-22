package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Transcript search (ctrl+f). Long sessions scroll past the point where paging
// is useful; this finds a line and puts the viewport on it. Matching is done on
// the plain text of each rendered line, so styling never breaks a match.

type search struct {
	active  bool // the query prompt has focus
	query   string
	matches []int // transcript line indices, ascending
	at      int   // index into matches
}

// openSearch starts a query, seeded with the previous one so ctrl+f, enter
// repeats the last search.
func (m *Model) openSearch() {
	m.search.active = true
	m.sel.active = false
	m.viewport.ClearHighlights()
}

func (m *Model) closeSearch(keepMatches bool) {
	m.search.active = false
	if !keepMatches {
		m.search = search{}
	}
	m.refreshTranscript()
}

// runSearch collects the matching line indices and jumps to the first one at or
// after the current scroll position, so searching mid-transcript moves forward
// rather than snapping to the top.
func (m *Model) runSearch() {
	m.search.matches = nil
	m.search.at = 0
	q := strings.ToLower(strings.TrimSpace(m.search.query))
	if q == "" {
		return
	}
	for i, line := range strings.Split(m.transcript.String(), "\n") {
		if strings.Contains(strings.ToLower(ansi.Strip(line)), q) {
			m.search.matches = append(m.search.matches, i)
		}
	}
	for i, line := range m.search.matches {
		if line >= m.viewport.YOffset() {
			m.search.at = i
			break
		}
	}
	m.showMatch()
}

// stepMatch moves to the next (+1) or previous (-1) match, wrapping around.
func (m *Model) stepMatch(delta int) {
	n := len(m.search.matches)
	if n == 0 {
		return
	}
	m.search.at = (m.search.at + delta + n) % n
	m.showMatch()
}

// showMatch scrolls the current match into view, a third of the way down so
// there's context above and below it.
func (m *Model) showMatch() {
	if len(m.search.matches) == 0 {
		return
	}
	line := m.search.matches[m.search.at]
	m.viewport.SetYOffset(max(line-m.viewport.Height()/3, 0))
	m.refreshTranscript()
}

// searchedLine reports whether a transcript line matched, and whether it's the
// one currently selected — layout()'s StyleLineFunc paints them.
func (m *Model) searchedLine(line int) (hit, current bool) {
	if m.search.query == "" {
		return false, false
	}
	for i, l := range m.search.matches {
		if l == line {
			return true, i == m.search.at
		}
	}
	return false, false
}

// searchBar is the prompt/status row shown above the input while searching.
func (m *Model) searchBar() string {
	if !m.search.active && len(m.search.matches) == 0 {
		return ""
	}
	c := m.mode.color()
	label := lipgloss.NewStyle().Background(c).Foreground(lipgloss.Color("232")).Bold(true).
		Padding(0, 1).Render("find")

	status := ""
	switch {
	case strings.TrimSpace(m.search.query) == "":
		status = "type to search"
	case len(m.search.matches) == 0:
		status = "no matches"
	default:
		status = fmt.Sprintf("%d/%d", m.search.at+1, len(m.search.matches))
	}
	hint := "enter next · shift+enter prev · esc close"
	if !m.search.active {
		hint = "n next · N prev · esc clear"
	}

	body := " " + m.search.query
	if m.search.active {
		body += "▌" // cursor
	}
	left := label + lipgloss.NewStyle().Background(surfaceColor).Foreground(textColor).
		Render(truncatePlain(body, max(m.width/2, 10)))

	// Narrow terminals lose the key hint, then the status — never the query.
	tail := status + " · " + hint + " "
	if lipgloss.Width(left)+lipgloss.Width(tail) > m.width {
		tail = status + " "
	}
	if lipgloss.Width(left)+lipgloss.Width(tail) > m.width {
		tail = ""
	}
	right := lipgloss.NewStyle().Background(surfaceColor).Foreground(lipgloss.Color("245")).Render(tail)
	pad := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	return left + lipgloss.NewStyle().Background(surfaceColor).Render(strings.Repeat(" ", pad)) + right
}

// updateSearch handles keys while the find prompt has focus.
func (m *Model) updateSearch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeSearch(false)
		m.layout()
		return m, nil
	case "enter":
		// Leave the prompt but keep the matches, so n/N keep working.
		m.search.active = false
		m.stepMatch(0)
		m.layout()
		return m, nil
	case "shift+enter":
		m.stepMatch(-1)
		return m, nil
	case "backspace":
		if m.search.query != "" {
			m.search.query = m.search.query[:len(m.search.query)-1]
			m.runSearch()
		}
		return m, nil
	case "down", "ctrl+n":
		m.stepMatch(1)
		return m, nil
	case "up", "ctrl+p":
		m.stepMatch(-1)
		return m, nil
	}
	if txt := msg.Text; txt != "" {
		m.search.query += txt
		m.runSearch()
	}
	return m, nil
}
