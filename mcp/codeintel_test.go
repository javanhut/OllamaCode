package mcp

import (
	"strings"
	"testing"
)

func TestFilterCodeMatches_DropsComments(t *testing.T) {
	in := strings.Join([]string{
		"foo.go:10:func Process() {",
		"foo.go:11:    // Process the items here",
		"bar.go:3:# Process config",
		"baz.go:7:    Process(x)",
		"doc.go:1: * Process is a function",
	}, "\n")
	out := filterCodeMatches(in, 50)
	if strings.Contains(out, "// Process") || strings.Contains(out, "# Process") || strings.Contains(out, "* Process") {
		t.Fatalf("comment lines should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "func Process()") || !strings.Contains(out, "Process(x)") {
		t.Fatalf("code lines should be kept:\n%s", out)
	}
}

func TestFilterCodeMatches_KeepsPointerAndMultiply(t *testing.T) {
	// "* " is a block-comment continuation, but "*p" / "a * b" are code.
	in := "x.go:1:\t*p = compute()\ny.go:2:\ttotal = a * b\n"
	out := filterCodeMatches(in, 50)
	if !strings.Contains(out, "*p = compute()") || !strings.Contains(out, "a * b") {
		t.Fatalf("pointer/multiply lines must be kept:\n%s", out)
	}
}

func TestFilterCodeMatches_Caps(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "f.go:1:code line")
	}
	out := filterCodeMatches(strings.Join(lines, "\n"), 3)
	if !strings.Contains(out, "truncated at 3") {
		t.Fatalf("expected truncation footer:\n%s", out)
	}
}
