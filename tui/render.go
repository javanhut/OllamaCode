package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamourAnsi "github.com/charmbracelet/glamour/ansi"
	glamourStyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/javanhut/ollama_code/tools"
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

// hunkRe matches the "@@ -old,n +new,m @@" header unifiedDiff emits, capturing
// the line each hunk starts at in the old and new file.
var hunkRe = regexp.MustCompile(`^@@ -(\d+),\d+ \+(\d+),\d+ @@`)

// colorizeDiff styles a unified-diff block: additions green, deletions red,
// file/hunk headers dim. Each line gets a line-number gutter — the new file's
// number, except deletions which show the old file's. Lines are truncated to
// width and the block is capped so one large edit can't flood the transcript.
func colorizeDiff(diff string, width int) string {
	const maxLines = 200
	const gutter = 5 // "1234 "
	if width < 20 {
		width = 20
	}
	body := max(width-gutter, 10)
	lines := strings.Split(diff, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	var b strings.Builder
	oldNo, newNo := 0, 0
	for _, ln := range lines {
		kind := diffLineKind(ln)
		num := ""
		switch kind {
		case 'm':
			if mt := hunkRe.FindStringSubmatch(ln); mt != nil {
				oldNo, _ = strconv.Atoi(mt[1])
				newNo, _ = strconv.Atoi(mt[2])
			}
		case '+':
			num = strconv.Itoa(newNo)
			newNo++
		case '-':
			num = strconv.Itoa(oldNo)
			oldNo++
		default:
			num = strconv.Itoa(newNo)
			oldNo++
			newNo++
		}
		b.WriteString(diffMetaStyle.Render(fmt.Sprintf("%4s ", num)))
		ln = truncatePlain(ln, body)
		switch kind {
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

// lastNonEmptyLine returns the last line of s with content, for one-line tickers.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			return ln
		}
	}
	return ""
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

// stripControl removes ANSI escape sequences and stray control characters
// (except \n and \t) so pasted text or tool output can't corrupt the terminal
// layout when echoed back into the transcript.
func stripControl(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r < ' ' && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// codeFence returns a backtick fence guaranteed to be longer than any
// backtick run in content, so content can never close its own code block.
func codeFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// fencedDump wraps raw content in a fenced code block so markdown
// metacharacters in tool output aren't re-parsed by the renderer. Lines are
// capped at maxLines and truncated to width cells.
func fencedDump(content string, maxLines, width int) string {
	content = stripControl(content)
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}

	var b strings.Builder
	b.WriteString(codeFence(content))
	for _, line := range lines {
		b.WriteString("\n" + ansi.Truncate(line, width, "..."))
	}
	if truncated {
		b.WriteString("\n…")
	}
	b.WriteString("\n" + codeFence(content))
	return b.String()
}

func renderCollapsedTool(call tools.ToolCall, content string, verbose bool, width int) string {
	status := "completed"
	if strings.HasPrefix(content, "error:") {
		status = "failed"
	}

	header := fmt.Sprintf("**›** `%s` (%s)", call.Function.Name, status)
	if !verbose {
		return header
	}

	return header + "\n" + fencedDump(content, 12, width)
}

func renderToolCall(call tools.ToolCall, verbose bool, width int) string {
	name := fmt.Sprintf("**›** `%s`", call.Function.Name)
	if !verbose {
		return name
	}
	args := stripControl(strings.TrimSpace(string(call.Function.Arguments)))
	if args == "" {
		args = "{}"
	}
	args = ansi.Truncate(strings.ReplaceAll(args, "\n", " "), width, "...")
	return name + "\n" + codeFence(args) + "\n" + args + "\n" + codeFence(args)
}

func renderToolResult(name, content string, verbose bool, width int) string {
	status := "completed"
	if strings.HasPrefix(content, "error:") {
		status = "failed"
	}

	header := fmt.Sprintf("**←** `%s` (%s)", name, status)
	if !verbose {
		return header
	}

	return header + "\n" + fencedDump(content, 12, width)
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

	// Split on ``` fences and rewrite math only in the non-code segments
	// (even indices), so $…$ inside code blocks stays literal.
	segments := strings.Split(s, "```")
	for i := 0; i < len(segments); i += 2 {
		segments[i] = rewriteMath(segments[i])
	}
	return strings.Join(segments, "```")
}

// rewriteMath applies the math replacements to a single non-code segment:
// multi-line $$ … $$ and \[ … \] blocks become code blocks, inline $ … $
// and \( … \) become inline code.
func rewriteMath(s string) string {
	s = regexp.MustCompile(`(?s)\$\$(.*?)\$\$`).ReplaceAllString(s, "```latex\n$1\n```")
	s = regexp.MustCompile(`(?s)\\\[(.*?)\\\]`).ReplaceAllString(s, "```latex\n$1\n```")
	s = regexp.MustCompile(`\\\((.*?)\\\)`).ReplaceAllString(s, "`$1`")
	s = mathInlineRe.ReplaceAllString(s, "`$1`")
	return s
}

// markdownRenderer wraps a glamour renderer with its own wrap width and render
// cache. Each panel (chat, notes) owns one so their differing widths don't
// thrash a shared renderer and blow away each other's cache.
type markdownRenderer struct {
	renderer *glamour.TermRenderer
	width    int
	cache    map[string]string
}

func newMarkdownRenderer() *markdownRenderer {
	return &markdownRenderer{cache: make(map[string]string)}
}

// render renders s (with LaTeX math stripped) at the given wrap width, rebuilding
// the underlying renderer and dropping the cache only when the width changes.
// Returns "" + false on renderer error so the caller can fall back to raw text.
func (r *markdownRenderer) render(s string, wrap int, useCache bool) (string, bool) {
	if useCache {
		if cached, ok := r.cache[s]; ok {
			return cached, true
		}
	}
	if r.renderer == nil || r.width != wrap {
		tr, err := glamour.NewTermRenderer(
			glamour.WithStyles(laylaMarkdownStyle()),
			glamour.WithWordWrap(wrap),
		)
		if err != nil {
			return "", false
		}
		r.renderer = tr
		r.width = wrap
		r.cache = make(map[string]string)
	}
	out, err := r.renderer.Render(stripLatexMath(s))
	if err != nil {
		return "", false
	}
	res := strings.TrimRight(out, "\n")
	if useCache {
		r.cache[s] = res
	}
	return res, true
}

// reset drops the renderer and cache, forcing a rebuild on next render (e.g. on
// a window resize, when the style/width baseline changes).
func (r *markdownRenderer) reset() {
	r.renderer = nil
	r.cache = make(map[string]string)
}

func (m *Model) renderMarkdown(s string, useCache bool) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	width := m.viewport.Width()
	if width <= 4 {
		width = 80
	}
	if out, ok := m.md.render(s, width-2, useCache); ok {
		return out
	}
	return s
}

func (m *Model) renderNotesMarkdown(s string, width int) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	if out, ok := m.notesMd.render(s, width-2, true); ok {
		return out
	}
	return s
}

// LaTeX math notation patterns. We rewrite $…$ / $$…$$ as inline code so
// the user sees a styled span instead of literal dollar signs (glamour has no
// math renderer). Currency like "$5" doesn't match because it has no closer.
var (
	mathDisplayRe = regexp.MustCompile(`\$\$([^\n$]+?)\$\$`)
	mathInlineRe  = regexp.MustCompile(`\$([^\s$](?:[^$\n]*?[^\s$])?)\$`)
)
