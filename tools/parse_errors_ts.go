//go:build treesitter

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/javanhut/ollama_code/internal/tslang"
)

// tsParseErrors reports syntax errors in path as "path:line: message" lines,
// or nil when the file's language has no compiled grammar or the file cannot
// be read. Only ERROR and missing nodes are reported, and descent stops at
// the first error node — its children add location noise, not signal. Each
// call parses fresh: the verify gate runs this once per turn on the handful
// of changed files, so sharing the code-intelligence parse cache would buy
// nothing and couple this to it.
func tsParseErrors(path string) []string {
	lang, ok := tslang.ForExt(filepath.Ext(path))
	if !ok {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tslang.Mu.Lock()
	defer tslang.Mu.Unlock()
	tree := lang.Parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	if !tree.RootNode().HasError() {
		return nil
	}
	lines := strings.Split(string(src), "\n")
	var errs []string
	var walk func(n *tree_sitter.Node)
	walk = func(n *tree_sitter.Node) {
		if len(errs) >= maxParseErrors {
			return
		}
		if n.IsError() || n.IsMissing() {
			row := int(n.StartPosition().Row)
			line := ""
			if row < len(lines) {
				line = strings.TrimSpace(lines[row])
			}
			if len(line) > 60 {
				line = line[:60] + "…"
			}
			errs = append(errs, fmt.Sprintf("%s:%d: syntax error near %q", path, row+1, line))
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	return errs
}
