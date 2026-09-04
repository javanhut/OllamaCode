package tui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javanhut/ollama_code/api"
)

// Verified citations (explore mode).
//
// The explore-mode prompt requires every claim about the code to carry an
// inline `path:line` citation. When an explore answer finalizes, the citation
// gate (maybeCitationGate, wired into the chatDoneMsg path in update.go like
// the verify gate) parses the answer, resolves each citation against the
// workspace, and checks that the file exists and the line is in range. An
// answer that names source files but cites none, or whose citations do not
// resolve, earns ONE corrective system message and a re-invoke. Loop safety
// needs no Model field: the correction is detectable in the history tail (see
// citationCorrectionIssued), and it is issued at most once per turn — the
// retried answer is accepted as-is.
//
// The "makes code claims" heuristic (makesCodeClaims) is deliberately
// conservative: an answer is only punished for having no citations when it
// names at least one plausible source file (a token ending in a known code
// extension). Pure explanations — "what is a mutex", "how does backoff work"
// — name no files and are never challenged.

// citationCorrectionPrefix marks the corrective message in history so the gate
// can tell a correction was already issued this turn without any Model state.
const citationCorrectionPrefix = "[CITATION CHECK]"

// citationRe matches `path:line` and `path:line-line` references. The path
// character class is greedy over letters, digits, and the usual path
// punctuation; the colon and digits anchor the match. Whether a match is
// really a citation (and not a time, a URL, or prose like "note: 5") is
// decided by the filters in parseCitations.
var citationRe = regexp.MustCompile(`([A-Za-z0-9_~./\\-]+):(\d+)(?:-(\d+))?`)

// codeClaimRe is the conservative "names a source file" heuristic: a
// word/path token ending in a known code extension. It intentionally ignores
// bare identifiers and function names — mentioning `parseConfig` is not a
// code claim, mentioning `tui/mode.go` is.
var codeClaimRe = regexp.MustCompile(`(?i)\b[\w./\\-]+\.(?:go|py|pyi|js|jsx|ts|tsx|mjs|cjs|rs|c|h|cc|cpp|cxx|hpp|java|kt|kts|rb|php|swift|scala|sql|sh|bash|zsh|fish|json|yaml|yml|toml|xml|html|css|scss|less|md|proto|lua|pl|pm|ex|exs|erl|hrl|hs|ml|mli|fs|fsx|cs|vue|svelte|dart|groovy)\b`)

// citation is one parsed `path:line` (or `path:line-line`) reference.
type citation struct {
	Raw     string // exact matched text, e.g. "tui/mode.go:42" — used for rendering
	Path    string // path portion, e.g. "tui/mode.go"
	Line    int    // 1-based start line
	EndLine int    // 0 for a single-line citation, else the inclusive end line
}

func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isPathByte(b byte) bool {
	return isASCIILetter(b) || b >= '0' && b <= '9' ||
		b == '_' || b == '~' || b == '.' || b == '/' || b == '\\' || b == '-'
}

// parseCitations extracts every `path:line` / `path:line-line` citation from
// text, in order of appearance, deduplicated by raw text. It rejects:
//
//   - bare numbers ("10:30", "ratio 2:1") — the path must contain a letter;
//   - prose keys ("note: 5") — the path must also contain a /, \, or .;
//   - URLs ("https://x.test/a.go:10") — anything preceded by a scheme colon;
//   - markdown link targets ("[text](a.go:10)") — the visible text is what
//     counts, and link targets here are almost always external.
//
// Windows-ish paths get their drive letter recovered: the path class excludes
// the drive colon, so `C:\src\a.go:10` matches from the backslash and the
// `C:` prefix is re-attached when it is a lone letter (not a longer path's
// tail, which is how URLs are told apart).
func parseCitations(text string) []citation {
	var out []citation
	seen := make(map[string]bool)
	for _, m := range citationRe.FindAllStringSubmatchIndex(text, -1) {
		start := m[0]
		path := text[m[2]:m[3]]

		// A colon immediately before the match means it trailed a scheme
		// ("https:…"), a drive letter ("C:…"), or prose — never a clean
		// citation start. Recover the drive-letter case, skip the rest.
		if start >= 1 && text[start-1] == ':' {
			if start >= 2 && isASCIILetter(text[start-2]) && (start < 3 || !isPathByte(text[start-3])) {
				path = text[start-2:start] + path
			} else {
				continue
			}
		}
		if strings.Contains(path, "://") {
			continue
		}
		// Markdown link target: the match begins right after "](".
		if start >= 2 && text[start-1] == '(' && text[start-2] == ']' {
			continue
		}
		if !hasASCIILetter(path) || !strings.ContainsAny(path, `/.\`) {
			continue
		}
		line, err := strconv.Atoi(text[m[4]:m[5]])
		if err != nil {
			continue
		}
		end := 0
		if m[6] >= 0 {
			if e, err := strconv.Atoi(text[m[6]:m[7]]); err == nil {
				end = e
			}
		}
		raw := text[m[0]:m[1]]
		if seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, citation{Raw: raw, Path: path, Line: line, EndLine: end})
	}
	return out
}

func hasASCIILetter(s string) bool {
	for i := 0; i < len(s); i++ {
		if isASCIILetter(s[i]) {
			return true
		}
	}
	return false
}

// makesCodeClaims reports whether an answer names at least one source file that
// RESOLVES against the workspace. Conservative on purpose: it is the only
// trigger for the "missing citations" correction, so answers that explain
// concepts without pointing at files pass through unchallenged.
//
// Existence is the load-bearing half. A file that isn't in the workspace has no
// citable line — validateCitations rejects any `path:line` naming it — so
// demanding a citation for it asks for something that cannot be produced. The
// answer "there is no index.html yet" was challenged on exactly that basis, and
// the only way to comply was to delete the true sentence. Naming a file that
// isn't there is not a claim about code that exists; the gate stays out of it.
func makesCodeClaims(root, answer string) bool {
	for _, named := range codeClaimRe.FindAllString(answer, -1) {
		path := named
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// validateCitations resolves each citation against root and returns one
// human-readable problem per citation that fails, or nil when every citation
// checks out. Relative paths join root; absolute paths are checked as-is.
func validateCitations(root string, cites []citation) []string {
	lineCounts := make(map[string]int)
	lineCount := func(abs string) (int, bool) {
		if n, ok := lineCounts[abs]; ok {
			return n, true
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return 0, false
		}
		n := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			n++ // final unterminated line still counts
		}
		lineCounts[abs] = n
		return n, true
	}

	var problems []string
	seen := make(map[string]bool)
	for _, c := range cites {
		if seen[c.Raw] {
			continue
		}
		seen[c.Raw] = true
		if c.EndLine != 0 && c.EndLine < c.Line {
			problems = append(problems, fmt.Sprintf("%s — reversed line range", c.Raw))
			continue
		}
		abs := c.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}
		n, ok := lineCount(abs)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s — file does not exist in the workspace", c.Raw))
			continue
		}
		end := c.Line
		if c.EndLine != 0 {
			end = c.EndLine
		}
		if c.Line < 1 || end > n {
			problems = append(problems, fmt.Sprintf("%s — line out of range (%s has %d lines)", c.Raw, c.Path, n))
		}
	}
	return problems
}

// citationProblems is the gate's pure core: every reason the answer's
// citations fail verification, or nil when the answer is fine as-is.
func citationProblems(root, answer string) []string {
	cites := parseCitations(answer)
	if len(cites) == 0 {
		if !makesCodeClaims(root, answer) {
			return nil
		}
		return []string{"the answer names source files but backs none of the claims with a path:line citation"}
	}
	return validateCitations(root, cites)
}

// citationCorrectionIssued reports whether a citation correction already
// appears in the current turn's history tail (everything since the last user
// message). This is the loop guard: the gate fires at most once per turn
// without needing any per-turn Model field.
func citationCorrectionIssued(history []api.Message) bool {
	for _, msg := range slices.Backward(history) {

		if isUserTurn(msg) {
			return false
		}
		if msg.Role == "system" && strings.HasPrefix(msg.Content, citationCorrectionPrefix) {
			return true
		}
	}
	return false
}

// citationCorrectionMessage builds the one corrective system message handed
// back to the model, listing exactly which citations failed and why.
func citationCorrectionMessage(problems []string) string {
	const maxListed = 8
	listed := problems
	extra := 0
	if len(listed) > maxListed {
		extra = len(listed) - maxListed
		listed = listed[:maxListed]
	}
	var b strings.Builder
	b.WriteString(citationCorrectionPrefix + " Your answer's citations could not be verified against the workspace:\n")
	for _, p := range listed {
		b.WriteString("- " + p + "\n")
	}
	if extra > 0 {
		fmt.Fprintf(&b, "- …and %d more\n", extra)
	}
	b.WriteString("\nRe-answer with an accurate path:line citation for every claim about the code (e.g. `tui/mode.go:42`, `api/api.go:120-135`). Open the file and use the line number you actually see — do not guess. Drop claims you cannot tie to a file you read, or mark them as general knowledge.")
	return b.String()
}

// maybeCitationGate runs when an explore-mode answer finalizes: append one
// corrective system message and re-invoke the model with tools suppressed,
// since the only acceptable reply is a better answer. Returns nil to let the
// turn end normally — for non-explore modes, empty answers, answers without
// code claims, answers whose citations all verify, and (the loop guards) turns
// stopped for stagnation or where a correction was already issued.
func (m *Model) maybeCitationGate(answer string) tea.Cmd {
	if m.mode != ExploreMode || strings.TrimSpace(answer) == "" {
		return nil
	}
	// A turn stopped for making no progress is over. Asking for a re-answer here
	// would hold it open for another round, which is the loop this guard ends.
	if m.endTurnAfterReply {
		return nil
	}
	if citationCorrectionIssued(m.history) {
		return nil
	}
	problems := citationProblems(workspaceRoot(), answer)
	if len(problems) == 0 {
		return nil
	}
	m.history = append(m.history, api.Message{Role: "system", Content: citationCorrectionMessage(problems)})
	// The gate wants a re-ANSWER, not more investigation. Left free to call
	// tools, a model answers the correction with a tool call and never comes
	// back — the burned session lost 35 rounds exactly there. startStream
	// records the withholding, so the demand is enforced, not requested.
	m.suppressToolsOnce = true
	m.busySince = time.Now()
	return m.startStream()
}

// citationStyle makes verified references visually distinct — underlined cyan,
// like a link. The printable text stays exactly `path:line`, so drag-select
// copy and ctrl+f search keep working on the plain form.
var citationStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Underline(true)

// sgrRe matches ANSI SGR sequences so restyleLineCitations can restore
// whatever styling was active before a styled citation reset it.
var sgrRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// styleCitations post-processes rendered (ANSI) markdown, restyling every
// citation it can find verbatim. Citations that glamour split across a wrap
// boundary are left plain — styling is best-effort decoration, never a
// content change.
func styleCitations(rendered string) string {
	if !strings.Contains(rendered, ":") {
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	changed := false
	for i, line := range lines {
		if !strings.Contains(line, ":") {
			continue
		}
		cites := parseCitations(ansi.Strip(line))
		if len(cites) == 0 {
			continue
		}
		// Longest raw first: `a.go:10-12` must be styled before `a.go:10`
		// can match inside it. The forward-only search in
		// restyleLineCitations then can't double-style the overlap.
		sort.SliceStable(cites, func(a, b int) bool { return len(cites[a].Raw) > len(cites[b].Raw) })
		lines[i] = restyleLineCitations(line, cites)
		changed = true
	}
	if !changed {
		return rendered
	}
	return strings.Join(lines, "\n")
}

// restyleLineCitations replaces each citation's raw text in a rendered line
// with its styled form, re-applying the SGR state that was active before each
// match so the styled citation's reset doesn't strip the rest of the line.
func restyleLineCitations(line string, cites []citation) string {
	out := line
	offset := 0
	for _, c := range cites {
		idx := strings.Index(out[offset:], c.Raw)
		if idx < 0 {
			continue
		}
		idx += offset
		styled := citationStyle.Render(c.Raw) + activeSGR(out[:idx])
		out = out[:idx] + styled + out[idx+len(c.Raw):]
		offset = idx + len(styled)
	}
	return out
}

// activeSGR replays the SGR sequences in prefix and returns the escape that
// restores the still-active attributes ("" when plain). A "0" parameter
// resets the accumulated state, matching terminal semantics.
func activeSGR(prefix string) string {
	var codes []string
	for _, seq := range sgrRe.FindAllString(prefix, -1) {
		params := seq[2 : len(seq)-1]
		if params == "" {
			params = "0"
		}
		for p := range strings.SplitSeq(params, ";") {
			if p == "0" {
				codes = codes[:0]
			} else {
				codes = append(codes, p)
			}
		}
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}
