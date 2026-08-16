package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// symbolPattern matches an identifier in any of the languages the code_* tools
// claim to handle.
var symbolPattern = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// defKeywords are the declaration keywords skipped when picking the symbol on
// a line: on `func handleRequest(` the interesting word is the second one.
var defKeywords = map[string]bool{
	"func": true, "fn": true, "def": true, "class": true,
	"struct": true, "type": true, "var": true, "const": true, "let": true,
	"return": true, "if": true, "for": true, "import": true, "pub": true,
}

// refKeywords additionally skips the control-flow words that show up on the
// call sites a references query is usually pointed at.
var refKeywords = func() map[string]bool {
	m := map[string]bool{"package": true, "else": true, "match": true, "switch": true, "case": true}
	for k := range defKeywords {
		m[k] = true
	}
	return m
}()

// symbolAt picks the likely symbol on a line — the last identifier that is not
// a language keyword — and returns it with its one-based column.
//
// One decision, two consumers: the grep tier searches for the name and the LSP
// tier queries the position. Deriving them separately is how they would drift,
// and a definition lookup that answers about a different symbol than the one
// the tool reported is worse than no answer at all.
func symbolAt(line string, keywords map[string]bool) (sym string, col int, ok bool) {
	spans := symbolPattern.FindAllStringIndex(line, -1)
	if len(spans) == 0 {
		return "", 0, false
	}
	pick := spans[len(spans)-1]
	for i := len(spans) - 1; i >= 0; i-- {
		if !keywords[line[spans[i][0]:spans[i][1]]] {
			pick = spans[i]
			break
		}
	}
	return line[pick[0]:pick[1]], pick[0] + 1, true
}

// isCommentLine reports whether a matched line's content is (heuristically) a
// comment rather than code, so symbol searches don't surface doc-comment noise.
// Conservative: only well-known comment openers, and "* " for block-comment
// continuations (so it won't drop pointer/multiply lines like "*p = x").
func isCommentLine(content string) bool {
	t := strings.TrimSpace(content)
	if t == "" {
		return false
	}
	switch {
	case strings.HasPrefix(t, "//"),
		strings.HasPrefix(t, "#"),
		strings.HasPrefix(t, "/*"),
		strings.HasPrefix(t, "--"),
		strings.HasPrefix(t, "* "),
		t == "*":
		return true
	}
	return false
}

// filterCodeMatches drops comment-only lines from grep-style `path:line:content`
// output and caps the result to limit lines with a truncation footer. This keeps
// symbol/definition/reference searches focused on real code and bounded in size.
func filterCodeMatches(text string, limit int) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		content := ln
		if parts := strings.SplitN(ln, ":", 3); len(parts) == 3 {
			content = parts[2]
		}
		if isCommentLine(content) {
			continue
		}
		kept = append(kept, ln)
	}
	truncated := 0
	if limit > 0 && len(kept) > limit {
		truncated = len(kept) - limit
		kept = kept[:limit]
	}
	out := strings.Join(kept, "\n")
	if truncated > 0 {
		out += fmt.Sprintf("\n\n... and %d more (truncated at %d; narrow your search)", truncated, limit)
	}
	return out
}
