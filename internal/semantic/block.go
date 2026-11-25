package semantic

import (
	"fmt"
	"strings"
)

// FormatBlock renders search results into a token-budgeted context block —
// the exact text the TUI injects each turn and cmd/eval injects under -rag on,
// so an eval A/B measures what users actually get. estimate is the caller's
// token estimator (the TUI's is calibrated per model). At least one result is
// always included when any exist, even if it alone exceeds the budget.
func FormatBlock(results []Result, maxTokens int, estimate func(string) int) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[RETRIEVED CONTEXT — auto-selected by relevance to your latest message; verify against the live files before trusting]\n")
	used := estimate(b.String())
	included := 0
	for _, r := range results {
		entry := fmt.Sprintf("\n%s:%d-%d (score %.3f)\n%s\n", r.Path, r.StartLine, r.EndLine, r.Score, r.Text)
		t := estimate(entry)
		if used+t > maxTokens && included > 0 {
			break
		}
		b.WriteString(entry)
		used += t
		included++
	}
	return b.String()
}
