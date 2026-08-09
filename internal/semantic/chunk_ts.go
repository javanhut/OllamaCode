//go:build treesitter

package semantic

import (
	"path/filepath"
	"sort"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/javanhut/ollama_code/internal/tslang"
)

// chunkerID tags indexes built by this binary's chunking scheme. With the
// treesitter tag, supported languages chunk on symbol boundaries, which is
// incompatible with a window-chunked cache — LoadIndex rejects the mismatch
// and callers rebuild.
const chunkerID = "symbols-v1"

// lineSpan is a 0-based, inclusive row range.
type lineSpan struct{ start, end int }

// symbolChunks parses src with tree-sitter and chunks on top-level symbol
// boundaries: each chunk covers one or more whole top-level definitions
// (functions, types, classes, ...) with their doc comments attached. Small
// adjacent symbols merge into one chunk up to chunkMaxLines so the embedding
// count stays near the window chunker's; an oversized single symbol splits
// into overlapping windows of the same size. ok=false routes the file to
// windowChunks: unsupported extension, parse failure, or a file with no
// top-level definitions at all (e.g. only imports).
func symbolChunks(rel, src string) ([]Chunk, bool) {
	lang, ok := tslang.ForExt(filepath.Ext(rel))
	if !ok {
		return nil, false
	}
	spans, ok := topLevelDefSpans(lang, src)
	if !ok {
		return nil, false
	}
	lines := strings.Split(src, "\n")
	last := len(lines) - 1
	var chunks []Chunk
	emit := func(start, end int) {
		if start > end {
			return
		}
		chunks = append(chunks, Chunk{
			Path:      rel,
			StartLine: start + 1,
			EndLine:   end + 1,
			Text:      strings.Join(lines[start:end+1], "\n"),
		})
	}
	cur := 0
	for _, sp := range spans {
		start, end := sp.start, min(sp.end, last)
		if end-start+1 > chunkMaxLines {
			// Oversized symbol: flush what precedes it, then split the
			// symbol itself into overlapping windows.
			if cur < start {
				emit(cur, start-1)
			}
			for i := start; ; i += chunkMaxLines - chunkOverlap {
				e := min(i+chunkMaxLines-1, end)
				emit(i, e)
				if e == end {
					break
				}
			}
			cur = end + 1
			continue
		}
		if end-cur+1 > chunkMaxLines && cur < start {
			// Adding this symbol would overflow the budget: close the
			// current chunk just before it and start fresh at the symbol.
			emit(cur, start-1)
			cur = start
		}
	}
	if cur <= last {
		emit(cur, last)
	}
	return chunks, true
}

// topLevelDefSpans parses src and returns the sorted, non-overlapping row
// spans of the file's top-level definitions, each extended upward over its
// contiguous doc-comment run. Spans are keyed by the top-level ancestor of
// each @def capture, so multi-name declarations (a Go var block, a Python
// module assignment) yield one span for the whole enclosing declaration.
func topLevelDefSpans(lang *tslang.Language, src string) ([]lineSpan, bool) {
	text := []byte(src)
	tslang.Mu.Lock()
	defer tslang.Mu.Unlock()
	tree := lang.Parser.Parse(text, nil)
	if tree == nil {
		return nil, false
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil || root.HasError() {
		return nil, false
	}
	lines := strings.Split(src, "\n")
	byRow := map[int]lineSpan{}
	for _, q := range lang.DefQueries {
		defIdx, hasDef := q.CaptureIndexForName("def")
		if !hasDef {
			continue
		}
		cur := tree_sitter.NewQueryCursor()
		ms := cur.Matches(q, root, text)
		for m := ms.Next(); m != nil; m = ms.Next() {
			for i := range m.Captures {
				c := &m.Captures[i]
				if uint(c.Index) != defIdx {
					continue
				}
				// Ascend to the direct child of the root so the span
				// covers the whole declaration (e.g. `type (...)` block,
				// decorated Python def), not just the inner spec node.
				top := c.Node
				for {
					p := top.Parent()
					if p == nil || p.Parent() == nil {
						break
					}
					top = *p
				}
				start := docCommentStart(lines, int(top.StartPosition().Row))
				row := int(top.StartPosition().Row)
				if prev, ok := byRow[row]; ok && prev.end >= int(top.EndPosition().Row) {
					continue
				}
				byRow[row] = lineSpan{start: start, end: int(top.EndPosition().Row)}
			}
		}
		cur.Close()
	}
	if len(byRow) == 0 {
		return nil, false
	}
	spans := make([]lineSpan, 0, len(byRow))
	for _, sp := range byRow {
		spans = append(spans, sp)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans, true
}

// docCommentStart extends row upward over the contiguous run of line
// comments immediately above it. Only "//" and "#" lines attach — any blank
// or code line breaks the run, which keeps license headers at the top of a
// file from sticking to the first definition. Same rule as the
// code-intelligence tools' tsDocRows.
func docCommentStart(lines []string, row int) int {
	for row > 0 {
		t := strings.TrimSpace(lines[row-1])
		if !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "#") {
			break
		}
		row--
	}
	return row
}
