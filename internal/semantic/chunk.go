package semantic

import "strings"

// Chunking budget, shared by the window chunker and the symbol-aware
// chunker (treesitter build tag). A chunk is at most chunkMaxLines lines;
// when windowing splits a region, consecutive windows overlap by
// chunkOverlap lines so retrieval doesn't lose split-point context.
const (
	chunkMaxLines = 100
	chunkOverlap  = 20
)

// chunkerWindow identifies indexes chunked with the plain line-window
// scheme. Indexes saved before the Chunker field existed have an empty
// value, which means exactly this scheme.
const chunkerWindow = "window-v1"

// chunkFile splits a file's contents into chunks. With the treesitter build
// tag and a supported language, chunks align to top-level symbol boundaries
// (see chunk_ts.go); every other case — the default build, unsupported
// languages, parse failures — gets the historical overlapping line windows.
// Returns nil for binary files.
func chunkFile(rel string, data []byte) []Chunk {
	if isBinary(data) {
		return nil
	}
	src := string(data)
	if chunks, ok := symbolChunks(rel, src); ok {
		return chunks
	}
	return windowChunks(rel, strings.Split(src, "\n"))
}

// windowChunks is the historical chunker: overlapping windows of
// chunkMaxLines lines advancing by chunkMaxLines-chunkOverlap.
func windowChunks(rel string, lines []string) []Chunk {
	var chunks []Chunk
	for i := 0; i < len(lines); i += chunkMaxLines - chunkOverlap {
		end := min(i+chunkMaxLines, len(lines))
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
