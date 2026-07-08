package tui

import (
	"fmt"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamourAnsi "github.com/charmbracelet/glamour/ansi"
	glamourStyles "github.com/charmbracelet/glamour/styles"
	"github.com/javanhut/ollama_code/mcp"
)

var (
	diffAddStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // additions: green
	diffDelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // deletions: red
	diffMetaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244")) // hunk/file headers: dim
)

// splitDiff separates a mutating-tool result into its human summary (everything
// before the unified-diff block) and the diff itself. diff is "" when the result
// carries no diff. Matches the "--- path" / "+++ path" header unifiedDiff emits.
func splitDiff(result string) (summary, diff string) {
	lines := strings.Split(result, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasPrefix(lines[i], "--- ") && strings.HasPrefix(lines[i+1], "+++ ") {
			return strings.TrimRight(strings.Join(lines[:i], "\n"), "\n"), strings.Join(lines[i:], "\n")
		}
	}
	return result, ""
}

// diffLineKind classifies a diff line: 'm' file/hunk header, '+' addition,
// '-' deletion, ' ' context. Header prefixes ("+++"/"---") are checked before
// the single "+"/"-" cases so they aren't miscolored as add/delete.
func diffLineKind(ln string) byte {
	switch {
	case strings.HasPrefix(ln, "@@"), strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
		return 'm'
	case strings.HasPrefix(ln, "+"):
		return '+'
	case strings.HasPrefix(ln, "-"):
		return '-'
	default:
		return ' '
	}
}

// colorizeDiff styles a unified-diff block: additions green, deletions red,
// file/hunk headers dim. Lines are truncated to width and the block is capped so
// one large edit can't flood the transcript.
func colorizeDiff(diff string, width int) string {
	const maxLines = 200
	if width < 20 {
		width = 20
	}
	lines := strings.Split(diff, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	var b strings.Builder
	for _, ln := range lines {
		ln = truncatePlain(ln, width)
		switch diffLineKind(ln) {
		case 'm':
			b.WriteString(diffMetaStyle.Render(ln))
		case '+':
			b.WriteString(diffAddStyle.Render(ln))
		case '-':
			b.WriteString(diffDelStyle.Render(ln))
		default:
			b.WriteString(mutedStyle.Render(ln))
		}
		b.WriteByte('\n')
	}
	if truncated {
		b.WriteString(diffMetaStyle.Render("… (diff truncated)"))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
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
