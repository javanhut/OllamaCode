//go:build !treesitter

package semantic

// chunkerID tags indexes built by this binary's chunking scheme. The default
// CGO-free build only knows line windows.
const chunkerID = chunkerWindow

// symbolChunks is compiled out in the default build; every file falls back
// to windowChunks. See chunk_ts.go for the tree-sitter implementation.
func symbolChunks(rel, src string) ([]Chunk, bool) {
	return nil, false
}
