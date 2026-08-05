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
	"unicode"

	"github.com/javanhut/ollama_code/internal/gitignore"
	"golang.org/x/sync/errgroup"
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
	var batchMeta []struct {
		idx   int
		start int
		end   int
		path  string
	}

	gi := gitignore.NewMatcher(root)

	var fileCount int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != root && gi.IsIgnored(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
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
			end := min(i+chunkSize, len(lines))
			text := strings.Join(lines[i:end], "\n")
			batch = append(batch, text)
			batchMeta = append(batchMeta, struct {
				idx, start, end int
				path            string
			}{idx: len(chunks), start: i + 1, end: end, path: rel})
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

	var g errgroup.Group
	g.SetLimit(4)

	batchSize := 10
	for i := 0; i < len(batch); i += batchSize {
		start := i
		end := min(i+batchSize, len(batch))
		g.Go(func() error {
			embs, err := embedder(batch[start:end])
			if err != nil {
				return fmt.Errorf("embedding batch %d-%d failed: %w", start, end, err)
			}
			for j, emb := range embs {
				meta := batchMeta[start+j]
				chunks[meta.idx].Embedding = emb
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return &Index{Root: root, Model: model, Chunks: chunks}, nil
}

// chunkFile splits a file's contents into overlapping line-windows, matching
// BuildIndex's chunking parameters. Returns nil for binary files.
func chunkFile(rel string, data []byte) []Chunk {
	if isBinary(data) {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	const chunkSize, overlap = 100, 20
	var chunks []Chunk
	for i := 0; i < len(lines); i += chunkSize - overlap {
		end := min(i+chunkSize, len(lines))
		chunks = append(chunks, Chunk{
			Path:      rel,
			StartLine: i + 1,
			EndLine:   end,
			Text:      strings.Join(lines[i:end], "\n"),
		})
		if end == len(lines) {
			break
		}
	}
	return chunks
}

// Clone returns a deep-enough copy: a new Index with a fresh Chunks slice. The
// per-chunk Embedding slices are shared (treated as immutable). Used so a
// background reindex can mutate a copy without racing readers of the published
// index.
func (idx *Index) Clone() *Index {
	out := &Index{Root: idx.Root, Model: idx.Model}
	out.Chunks = append([]Chunk(nil), idx.Chunks...)
	return out
}

// RemoveFile drops all chunks belonging to rel (relative path).
func (idx *Index) RemoveFile(rel string) {
	out := idx.Chunks[:0]
	for _, c := range idx.Chunks {
		if c.Path != rel {
			out = append(out, c)
		}
	}
	idx.Chunks = out
}

// ReindexFile re-embeds a single file: it drops the file's existing chunks and,
// if the file still exists and is non-binary, re-chunks and re-embeds it. A
// missing file is treated as a pure removal. Intended to be called on a cloned
// index from a background goroutine.
func (idx *Index) ReindexFile(root, rel string, embedder func([]string) ([][]float32, error)) error {
	idx.RemoveFile(rel)
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return nil // file gone/unreadable: removal only
	}
	chunks := chunkFile(rel, data)
	if len(chunks) == 0 {
		return nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	embs, err := embedder(texts)
	if err != nil {
		return err
	}
	for i := range chunks {
		if i < len(embs) {
			chunks[i].Embedding = embs[i]
		}
	}
	idx.Chunks = append(idx.Chunks, chunks...)
	return nil
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
	terms := queryTerms(query)
	for _, c := range idx.Chunks {
		if len(c.Embedding) == 0 {
			continue
		}
		semanticScore := cosineSimilarity(qemb, c.Embedding)
		lexicalScore := lexicalRelevance(terms, c.Path+"\n"+c.Text)
		// Semantic similarity supplies broad intent; exact identifiers, filenames,
		// and error text provide a precision boost that embeddings often blur.
		s := semanticScore*0.8 + lexicalScore*0.2
		results = append(results, scored{Chunk: c, score: s})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
	if topK > len(results) {
		topK = len(results)
	}
	out := make([]Result, 0, topK)
	perFile := map[string]int{}
	for _, result := range results {
		if len(out) >= topK {
			break
		}
		if perFile[result.Path] >= 2 {
			continue
		}
		perFile[result.Path]++
		out = append(out, Result{Chunk: result.Chunk, Score: result.score})
	}
	// If a small repository has fewer than topK results after diversification,
	// fill the remaining slots in score order.
	if len(out) < topK {
		seen := map[string]bool{}
		for _, result := range out {
			seen[fmt.Sprintf("%s:%d", result.Path, result.StartLine)] = true
		}
		for _, result := range results {
			key := fmt.Sprintf("%s:%d", result.Path, result.StartLine)
			if seen[key] {
				continue
			}
			out = append(out, Result{Chunk: result.Chunk, Score: result.score})
			if len(out) >= topK {
				break
			}
		}
	}
	return out, nil
}

func queryTerms(query string) []string {
	parts := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '/' || r == '-')
	})
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		if len(part) < 3 || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func lexicalRelevance(terms []string, candidate string) float64 {
	if len(terms) == 0 {
		return 0
	}
	candidate = strings.ToLower(candidate)
	matches := 0
	for _, term := range terms {
		if strings.Contains(candidate, term) {
			matches++
		}
	}
	return float64(matches) / float64(len(terms))
}

func isBinary(b []byte) bool {
	n := min(len(b), 512)
	for i := range n {
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
