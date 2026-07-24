package tui

import (
	"testing"

	"github.com/javanhut/ollama_code/api"
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
