package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func TestLastTurnDiffs(t *testing.T) {
	diffA := "--- a.go\n+++ a.go\n@@\n-old\n+new"
	diffB := "--- b.go\n+++ b.go\n@@\n-x\n+y"
	m := &Model{history: []api.Message{
		{Role: "user", Content: "old turn"},
		{Role: "tool", Content: "edited\n--- z.go\n+++ z.go\n@@\n-1\n+2"}, // prior turn: excluded
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "working"},
		{Role: "tool", Content: "edited a.go\n" + diffA},
		{Role: "tool", Content: "no diff here"},
		{Role: "tool", Content: "edited b.go\n" + diffB},
	}}
	got := m.lastTurnDiffs()
	want := diffA + "\n\n" + diffB
	if got != want {
		t.Fatalf("lastTurnDiffs() =\n%q\nwant\n%q", got, want)
	}
}

func TestSplitDiff(t *testing.T) {
	result := "edited main.go: replaced 1 occurrence(s)\nNew Hash: abc\n--- main.go\n+++ main.go\n@@\n-old\n+new"
	summary, diff := splitDiff(result)
	if summary != "edited main.go: replaced 1 occurrence(s)\nNew Hash: abc" {
		t.Fatalf("summary = %q", summary)
	}
	if diff != "--- main.go\n+++ main.go\n@@\n-old\n+new" {
		t.Fatalf("diff = %q", diff)
	}

	// No diff present -> whole thing is summary.
	if s, d := splitDiff("wrote 10 bytes to x\nNew Hash: z"); d != "" || s != "wrote 10 bytes to x\nNew Hash: z" {
		t.Fatalf("expected no diff, got summary=%q diff=%q", s, d)
	}
}

func TestStripLatexMath_SkipsFencedCode(t *testing.T) {
	// $…$ inside a fenced code block must stay literal; outside it rewrites.
	in := "outside $x + y$ here\n```\ninside $x + y$ code\n```"
	got := stripLatexMath(in)
	if !strings.Contains(got, "`x + y` here") {
		t.Fatalf("inline math outside fence not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "```\ninside $x + y$ code\n```") {
		t.Fatalf("math inside fenced code was rewritten:\n%s", got)
	}
}

func TestRenderCollapsedTool_FencedDump(t *testing.T) {
	call := tools.ToolCall{Function: tools.ToolCallFunction{Name: "run_shell"}}
	content := "# not a heading\n- not a list\nhas ``` backticks\n\x1b[31mred\x1b[0m"
	got := renderCollapsedTool(call, content, true, 80)
	// Raw markdown metacharacters must sit inside a fence longer than the
	// backtick run in the content, and ANSI escapes must be stripped.
	if !strings.Contains(got, "````\n# not a heading\n- not a list\nhas ``` backticks\nred\n````") {
		t.Fatalf("expected fenced, stripped dump, got:\n%s", got)
	}
	if strings.Contains(got, "> ") {
		t.Fatalf("dump still uses blockquote lines:\n%s", got)
	}
}

func TestDiffLineKind(t *testing.T) {
	cases := map[string]byte{
		"+++ main.go": 'm', // header must not read as addition
		"--- main.go": 'm',
		"@@":          'm',
		"+added":      '+',
		"-removed":    '-',
		" context":    ' ',
		"":            ' ',
	}
	for line, want := range cases {
		if got := diffLineKind(line); got != want {
			t.Errorf("diffLineKind(%q) = %c, want %c", line, got, want)
		}
	}
}

func TestStreamingTextUsesStablePlainRendering(t *testing.T) {
	m := &Model{mode: ExploreMode, md: newMarkdownRenderer()}
	m.viewport.SetWidth(80)
	turn := assistantTurn{
		streaming: true,
		segments:  []turnSegment{{live: true, text: "**done** paragraph\n\n# unfinished heading\n```go\nfunc main()"}},
	}
	var b strings.Builder
	m.writeAssistantTurn(&b, &turn, false)
	got := stripANSI(b.String())
	// The tail is still being written — reformatting it re-flows the layout on
	// every frame, so it stays raw until its block closes.
	if !strings.Contains(got, "# unfinished heading") || !strings.Contains(got, "```go") {
		t.Fatalf("streaming tail was reformatted before completion:\n%s", got)
	}
	// Everything above it is complete Markdown and renders as such.
	if strings.Contains(got, "**done**") {
		t.Fatalf("completed block was not rendered as Markdown:\n%s", got)
	}
}

func TestSplitStableMarkdown(t *testing.T) {
	cases := map[string][2]string{
		"para one\n\npara t":                       {"para one\n\n", "para t"},
		"intro\n\n```go\nfunc main() {\n\nx := 1":  {"intro\n\n", "```go\nfunc main() {\n\nx := 1"},
		"intro\n\n```go\nx\n```\n\ntail":           {"intro\n\n```go\nx\n```\n\n", "tail"},
		"# unfinished heading\n```go\nfunc main()": {"", "# unfinished heading\n```go\nfunc main()"},
		"": {"", ""},
	}
	for in, want := range cases {
		stable, tail := splitStableMarkdown(in)
		if stable != want[0] || tail != want[1] {
			t.Errorf("splitStableMarkdown(%q) = (%q, %q), want (%q, %q)", in, stable, tail, want[0], want[1])
		}
	}
}
