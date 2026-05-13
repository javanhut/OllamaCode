package semantic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Chunk struct {
	Path      string    `json:"path"`
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
}

type Index struct {
	Root   string  `json:"root"`
	Model  string  `json:"model"`
	Chunks []Chunk `json:"chunks"`
}

func cachePath(root string) string {
	h := sha256.Sum256([]byte(root))
	dir, _ := os.UserCacheDir()
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "ollama_code", "semantic_index", hex.EncodeToString(h[:])+".json")
}

func SaveIndex(idx *Index) error {
	path := cachePath(idx.Root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadIndex(root string) (*Index, error) {
	path := cachePath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func BuildIndex(root, model string, embedder func([]string) ([][]float32, error)) (*Index, error) {
	var chunks []Chunk
	var batch []string
	var batchMeta []struct{ idx int; start int; end int; path string }

	// Reuse skip dirs from mcp package logic
	skipDirs := map[string]bool{
		".git": true, ".svn": true, ".hg": true, ".bzr": true,
		"node_modules": true, "vendor": true, "target": true,
		"dist": true, "build": true, "out": true, "bin": true, "obj": true,
		"__pycache__": true, ".venv": true, "venv": true,
		".idea": true, ".vscode": true,
		".next": true, ".nuxt": true,
		"coverage": true, ".cache": true, ".terraform": true,
	}

	var fileCount int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		lines := strings.Split(string(data), "\n")
		chunkSize := 100
		overlap := 20
		for i := 0; i < len(lines); i += chunkSize - overlap {
			end := i + chunkSize
			if end > len(lines) {
				end = len(lines)
			}
			text := strings.Join(lines[i:end], "\n")
			batch = append(batch, text)
			batchMeta = append(batchMeta, struct{ idx, start, end int; path string }{idx: len(chunks), start: i + 1, end: end, path: rel})
			chunks = append(chunks, Chunk{Path: rel, StartLine: i + 1, EndLine: end, Text: text})
			if end == len(lines) {
				break
			}
		}
		fileCount++
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(batch) == 0 {
		return nil, fmt.Errorf("no text files found in %s", root)
	}

	batchSize := 10
	for i := 0; i < len(batch); i += batchSize {
		end := i + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		embs, err := embedder(batch[i:end])
		if err != nil {
			return nil, fmt.Errorf("embedding batch %d-%d failed: %w", i, end, err)
		}
		for j, emb := range embs {
			meta := batchMeta[i+j]
			chunks[meta.idx].Embedding = emb
		}
	}

	return &Index{Root: root, Model: model, Chunks: chunks}, nil
}

type Result struct {
	Chunk
	Score float64
}

func (idx *Index) Search(query string, embedder func(string) ([]float32, error), topK int) ([]Result, error) {
	qemb, err := embedder(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query failed: %w", err)
	}
	type scored struct {
		Chunk
		score float64
	}
	var results []scored
	for _, c := range idx.Chunks {
		if len(c.Embedding) == 0 {
			continue
		}
		s := cosineSimilarity(qemb, c.Embedding)
		results = append(results, scored{Chunk: c, score: s})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
	if topK > len(results) {
		topK = len(results)
	}
	out := make([]Result, topK)
	for i := 0; i < topK; i++ {
		out[i] = Result{Chunk: results[i].Chunk, Score: results[i].score}
	}
	return out, nil
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func cosineSimilarity(a, b []float32) float64 {
	var dot, na, nb float64
	for i := 0; i < len(a) && i < len(b); i++ {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
