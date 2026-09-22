package tools

import (
	"context"
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
	for range 10 {
		lines = append(lines, "f.go:1:code line")
	}
	out := filterCodeMatches(strings.Join(lines, "\n"), 3)
	if !strings.Contains(out, "truncated at 3") {
		t.Fatalf("expected truncation footer:\n%s", out)
	}
}

func TestLSPTierInertWhenDisabled(t *testing.T) {
	ConfigureLSP(false, ".", nil)
	t.Cleanup(func() { ConfigureLSP(false, ".", nil) })
	if _, ok := lspDefinitionAt(context.Background(), "main.go", 1, 1); ok {
		t.Fatal("disabled LSP tier answered a definition query")
	}
	if _, ok := lspHoverAt(context.Background(), "main.go", 1, 1); ok {
		t.Fatal("disabled LSP tier answered a hover query")
	}
	if got := LSPDiagnostics(context.Background(), []string{"main.go"}); got != "" {
		t.Fatalf("disabled LSP tier reported diagnostics: %q", got)
	}
}

func TestSymbolAtSkipsKeywordsAndReportsColumn(t *testing.T) {
	sym, col, ok := symbolAt("func handleRequest(w http.ResponseWriter) {", defKeywords)
	if !ok || sym != "ResponseWriter" {
		t.Fatalf("got %q ok=%v", sym, ok)
	}
	// Column is one-based and points at the symbol, which is what the LSP tier
	// converts to a position; an off-by-one here asks about the wrong token.
	if line := "func handleRequest(w http.ResponseWriter) {"; line[col-1:col-1+len(sym)] != sym {
		t.Fatalf("column %d does not point at %q", col, sym)
	}
	if _, _, ok := symbolAt("   ...   ", defKeywords); ok {
		t.Fatal("a line with no identifier should report no symbol")
	}
}
