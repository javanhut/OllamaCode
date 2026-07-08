package tui

import "testing"

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
