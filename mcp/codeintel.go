package mcp

import (
	"fmt"
	"strings"
)

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
