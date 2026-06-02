package semantic

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeEmbedder returns a deterministic 2-d vector per input so tests don't need
// a live Ollama. The vector encodes length so different texts differ.
func fakeEmbedder(in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i, s := range in {
		out[i] = []float32{float32(len(s)), 1}
	}
	return out, nil
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveFile(t *testing.T) {
	idx := &Index{Chunks: []Chunk{
		{Path: "a.go", Text: "x"},
		{Path: "b.go", Text: "y"},
		{Path: "a.go", Text: "z"},
	}}
	idx.RemoveFile("a.go")
	if len(idx.Chunks) != 1 || idx.Chunks[0].Path != "b.go" {
		t.Fatalf("expected only b.go to remain, got %+v", idx.Chunks)
	}
}

func TestCloneIsolation(t *testing.T) {
	idx := &Index{Root: "/r", Model: "m", Chunks: []Chunk{{Path: "a", Text: "1"}}}
	c := idx.Clone()
	c.RemoveFile("a")
	if len(idx.Chunks) != 1 {
		t.Fatal("mutating clone must not affect original")
	}
	if c.Root != "/r" || c.Model != "m" {
		t.Fatal("clone must preserve Root/Model")
	}
}

func TestReindexFile_AddsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.go", "package x\nfunc A(){}\n")
	idx := &Index{Root: dir, Model: "m"}

	if err := idx.ReindexFile(dir, "f.go", fakeEmbedder); err != nil {
		t.Fatal(err)
	}
	if len(idx.Chunks) == 0 {
		t.Fatal("expected chunks after reindex")
	}
	for _, c := range idx.Chunks {
		if len(c.Embedding) == 0 {
			t.Fatal("expected embeddings to be populated")
		}
	}

	// Re-running should replace, not duplicate.
	before := len(idx.Chunks)
	if err := idx.ReindexFile(dir, "f.go", fakeEmbedder); err != nil {
		t.Fatal(err)
	}
	if len(idx.Chunks) != before {
		t.Fatalf("reindex duplicated chunks: %d -> %d", before, len(idx.Chunks))
	}
}

func TestReindexFile_MissingFileRemoves(t *testing.T) {
	dir := t.TempDir()
	idx := &Index{Root: dir, Model: "m", Chunks: []Chunk{{Path: "gone.go", Text: "x"}}}
	if err := idx.ReindexFile(dir, "gone.go", fakeEmbedder); err != nil {
		t.Fatal(err)
	}
	if len(idx.Chunks) != 0 {
		t.Fatalf("missing file should leave no chunks, got %d", len(idx.Chunks))
	}
}
