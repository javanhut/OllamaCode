//go:build treesitter

package semantic

import (
	"fmt"
	"strings"
	"testing"
)

// goFuncFile builds a syntactically valid Go source file with n small
// functions, each carrying a doc comment, bodyLines statements long.
func goFuncFile(n, bodyLines int) string {
	var b strings.Builder
	b.WriteString("package x\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\n// F%02d does things.\nfunc F%02d() int {\n", i, i)
		for j := 0; j < bodyLines; j++ {
			fmt.Fprintf(&b, "\tx += %d\n", j)
		}
		b.WriteString("\treturn x\n}\n")
	}
	return b.String()
}

// assertSymbolChunks checks the invariants symbol chunking must hold when no
// single symbol overflows the budget: full contiguous coverage of the file
// and every chunk within the line budget.
func assertSymbolChunks(t *testing.T, chunks []Chunk, totalLines int) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	if chunks[0].StartLine != 1 {
		t.Fatalf("first chunk starts at %d, want 1", chunks[0].StartLine)
	}
	for i, c := range chunks {
		if n := c.EndLine - c.StartLine + 1; n > chunkMaxLines {
			t.Fatalf("chunk %d is %d lines, budget %d", i, n, chunkMaxLines)
		}
		if i > 0 && c.StartLine != chunks[i-1].EndLine+1 {
			t.Fatalf("chunk %d starts at %d, previous ended at %d: coverage gap/overlap",
				i, c.StartLine, chunks[i-1].EndLine)
		}
	}
	if got := chunks[len(chunks)-1].EndLine; got != totalLines {
		t.Fatalf("last chunk ends at %d, file has %d lines", got, totalLines)
	}
}

// Twelve small functions must merge into a couple of chunks (the embedding
// count stays near the window chunker's), and every break must land on a
// symbol boundary with its doc comment attached.
func TestSymbolChunksMergeSmallFunctions(t *testing.T) {
	src := goFuncFile(12, 11) // 194 lines
	chunks, ok := symbolChunks("f.go", src)
	if !ok {
		t.Fatal("expected symbol chunking for valid Go")
	}
	if len(chunks) > 3 {
		t.Fatalf("12 tiny functions produced %d chunks; merging failed", len(chunks))
	}
	assertSymbolChunks(t, chunks, 194)
	for i, c := range chunks {
		first := strings.TrimSpace(strings.SplitN(c.Text, "\n", 2)[0])
		if i == 0 {
			if first != "package x" {
				t.Fatalf("first chunk should start at the package clause, got %q", first)
			}
			continue
		}
		if !strings.HasPrefix(first, "// F") {
			t.Fatalf("chunk %d should start at a symbol's doc comment, got %q", i, first)
		}
	}
	// A break must not separate a doc comment from its function: the second
	// chunk's comment names the function defined in the same chunk.
	if len(chunks) >= 2 {
		name := strings.Split(strings.Split(chunks[1].Text, "\n")[0], " ")[1] // "//" "F06" ...
		if !strings.Contains(chunks[1].Text, "func "+name+"()") {
			t.Fatalf("doc comment for %s detached from its definition:\n%s", name, chunks[1].Text[:80])
		}
	}
}

// A single oversized symbol splits into overlapping windows of the line
// budget; the following small symbol stays a separate, whole chunk.
func TestSymbolChunksSplitOversizedSymbol(t *testing.T) {
	var b strings.Builder
	b.WriteString("package x\n\n// Big is huge.\nfunc Big() {\n")
	for j := 0; j < 160; j++ {
		b.WriteString("\tprintln(1)\n")
	}
	b.WriteString("}\n\nfunc small() {}\n")
	src := b.String()
	totalLines := len(strings.Split(src, "\n"))

	chunks, ok := symbolChunks("f.go", src)
	if !ok {
		t.Fatal("expected symbol chunking for valid Go")
	}
	for i, c := range chunks {
		if n := c.EndLine - c.StartLine + 1; n > chunkMaxLines {
			t.Fatalf("chunk %d is %d lines, budget %d", i, n, chunkMaxLines)
		}
	}
	if chunks[0].Text != "package x\n" {
		t.Fatalf("preamble should be its own chunk, got %q", chunks[0].Text)
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(last.Text, "func small() {}") {
		t.Fatalf("small function after the oversized one should end the file, got %q", last.Text)
	}
	if last.EndLine != totalLines {
		t.Fatalf("last chunk ends at %d, file has %d lines", last.EndLine, totalLines)
	}
	// The split windows of Big overlap so split-point context survives.
	if len(chunks) < 3 {
		t.Fatalf("expected preamble + >=2 windows + tail, got %d chunks", len(chunks))
	}
	if chunks[2].StartLine >= chunks[1].EndLine {
		t.Fatalf("oversized-symbol windows should overlap: %d-%d then %d-%d",
			chunks[1].StartLine, chunks[1].EndLine, chunks[2].StartLine, chunks[2].EndLine)
	}
}

// Files in unsupported languages fall back to the line windows.
func TestSymbolChunksUnsupportedLanguageFallsBack(t *testing.T) {
	src := goFuncFile(12, 11) // 194 lines of Go, but a .c name
	chunks := chunkFile("f.c", []byte(src))
	if len(chunks) != 3 || chunks[1].StartLine != 81 {
		t.Fatalf("expected window fallback (chunks at 1, 81, 161), got %+v", chunkBounds(chunks))
	}
}

// Syntactically invalid files fall back to the line windows.
func TestSymbolChunksParseErrorFallsBack(t *testing.T) {
	var b strings.Builder
	b.WriteString("package x\n\nfunc broken( {\n")
	for j := 0; j < 150; j++ {
		b.WriteString("\tx x x\n")
	}
	chunks := chunkFile("bad.go", []byte(b.String()))
	if len(chunks) < 2 || chunks[1].StartLine != 81 {
		t.Fatalf("expected window fallback on parse error, got %+v", chunkBounds(chunks))
	}
}

// A supported-language file with no top-level definitions (only package
// clause and comments) falls back to the line windows.
func TestSymbolChunksNoDefinitionsFallsBack(t *testing.T) {
	var b strings.Builder
	b.WriteString("package x\n")
	for j := 0; j < 150; j++ {
		fmt.Fprintf(&b, "// comment %d\n", j)
	}
	chunks := chunkFile("doc.go", []byte(b.String()))
	if len(chunks) < 2 || chunks[1].StartLine != 81 {
		t.Fatalf("expected window fallback with no definitions, got %+v", chunkBounds(chunks))
	}
}

func chunkBounds(chunks []Chunk) [][2]int {
	out := make([][2]int, len(chunks))
	for i, c := range chunks {
		out[i] = [2]int{c.StartLine, c.EndLine}
	}
	return out
}

func TestChunkerCompatTreeSitterBuild(t *testing.T) {
	if chunkerID != "symbols-v1" {
		t.Fatalf("treesitter build chunkerID = %q, want symbols-v1", chunkerID)
	}
	if err := chunkerCompat("symbols-v1"); err != nil {
		t.Fatalf("matching index should load: %v", err)
	}
	// Window-chunked caches — including legacy ones with no scheme recorded —
	// must be rejected so they are rebuilt on symbol boundaries.
	if err := chunkerCompat(chunkerWindow); err == nil {
		t.Fatal("window-chunked index must be rejected by the treesitter build")
	}
	if err := chunkerCompat(""); err == nil {
		t.Fatal("legacy index must be rejected by the treesitter build")
	}
}
