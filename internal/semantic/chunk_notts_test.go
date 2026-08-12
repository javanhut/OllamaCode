//go:build !treesitter

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
	for i := range n {
		fmt.Fprintf(&b, "\n// F%02d does things.\nfunc F%02d() int {\n", i, i)
		for j := range bodyLines {
			fmt.Fprintf(&b, "\tx += %d\n", j)
		}
		b.WriteString("\treturn x\n}\n")
	}
	return b.String()
}

// Without the treesitter tag, even a supported language gets the historical
// overlapping line windows.
func TestChunkFileWindowChunkingDefaultBuild(t *testing.T) {
	src := goFuncFile(12, 11) // 194 lines
	chunks := chunkFile("f.go", []byte(src))
	if len(chunks) != 3 {
		t.Fatalf("expected 3 window chunks, got %d", len(chunks))
	}
	want := [][2]int{{1, 100}, {81, 180}, {161, 194}}
	for i, w := range want {
		if chunks[i].StartLine != w[0] || chunks[i].EndLine != w[1] {
			t.Fatalf("chunk %d: want lines %d-%d, got %d-%d", i, w[0], w[1], chunks[i].StartLine, chunks[i].EndLine)
		}
	}
}

func TestChunkerCompatDefaultBuild(t *testing.T) {
	if chunkerID != chunkerWindow {
		t.Fatalf("default build chunkerID = %q, want %q", chunkerID, chunkerWindow)
	}
	// Legacy (empty) and window-schemed caches load; a symbol-chunked cache
	// must be rejected so it gets rebuilt with windows.
	if err := chunkerCompat(""); err != nil {
		t.Fatalf("legacy index should load: %v", err)
	}
	if err := chunkerCompat(chunkerWindow); err != nil {
		t.Fatalf("window index should load: %v", err)
	}
	if err := chunkerCompat("symbols-v1"); err == nil {
		t.Fatal("symbol-chunked index must be rejected by the default build")
	}
}
