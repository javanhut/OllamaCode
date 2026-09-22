//go:build treesitter

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/javanhut/ollama_code/internal/tslang"
)

// Tree-sitter precision path for the code-intelligence tools, enabled by the
// `treesitter` build tag (make build-ts). The default build compiles
// codeintel_fallback.go instead, which keeps the historical grep/regex
// behavior and — critically — keeps CGO and the grammar C sources out of the
// default binary, matching how the Gio companion is a separate binary for the
// same dependency-weight reason.
//
// The shape of every tool result is unchanged: `path:line:content` lines with
// the same per-tool caps and the same truncation footer the regex path
// produces. What changes is how the lines are chosen. For files in a
// supported language the match set comes from the parsed AST, so
// commented-out code and string literals no longer show up as definitions or
// references, and definitions are found even where the regexes miss them
// (e.g. `func Hello()` — the fallback definition pattern demands whitespace
// or end-of-line after the name). Files in unsupported languages, and files
// that fail to parse, are answered with the same per-file grep the default
// build runs, so coverage never regresses when the tag is on.
//
// Speed comes from a grep prefilter: parsing a whole workspace per query
// would be wasteful, so a fixed-string `grep -l` nominates the handful of
// files that mention the symbol at all and only those are parsed. Parsed
// trees are cached keyed by absolute path and revalidated by (size, mtime),
// which is cheap and correct enough for an interactive session. The cache is
// bounded and reset wholesale when full — partial LRU bookkeeping costs more
// than re-parsing a file that turns out to be hot again — and eviction closes
// the C trees because the binding does not register finalizers.
//
// All parser, tree, and node access is serialized under tslang.Mu:
// tree-sitter parsers are not thread-safe, the node accessors read C memory,
// and tool handlers can run on separate goroutines. The grammar registry
// itself (specs, compiled parsers and queries) lives in internal/tslang,
// shared with the symbol-aware semantic chunker.

const tsCacheLimit = 64

// tsParsedFile is one cached parse. lines is pre-split so emitted matches can
// carry the same whole-line content grep would print.
type tsParsedFile struct {
	tree  *tree_sitter.Tree
	src   []byte
	lines []string
	size  int64
	mod   time.Time
}

// tsMatch is one output line before formatting.
type tsMatch struct {
	path    string
	line    int
	content string
}

var tsState = struct {
	cache map[string]*tsParsedFile
}{
	cache: map[string]*tsParsedFile{},
}

// tsParseLocked returns the cached parse of displayPath, parsing on a miss.
// Validity is (size, mtime): same idea as build systems, cheap to check, and
// a false hit requires changing a file without changing either. Caller holds
// tslang.Mu.
func tsParseLocked(displayPath string, lang *tslang.Language) (*tsParsedFile, bool) {
	info, err := os.Stat(displayPath)
	if err != nil {
		return nil, false
	}
	key, err := filepath.Abs(displayPath)
	if err != nil {
		key = displayPath
	}
	if pf, ok := tsState.cache[key]; ok && pf.size == info.Size() && pf.mod.Equal(info.ModTime()) {
		return pf, true
	}
	src, err := os.ReadFile(displayPath)
	if err != nil {
		return nil, false
	}
	tree := lang.Parser.Parse(src, nil)
	if tree == nil {
		return nil, false
	}
	if len(tsState.cache) >= tsCacheLimit {
		for _, old := range tsState.cache {
			old.tree.Close()
		}
		clear(tsState.cache)
	}
	pf := &tsParsedFile{
		tree:  tree,
		src:   src,
		lines: strings.Split(string(src), "\n"),
		size:  info.Size(),
		mod:   info.ModTime(),
	}
	tsState.cache[key] = pf
	return pf, true
}

// tsCandidateFiles lists workspace files that mention sym at all, using the
// same exclusion set as the regex path. Fixed-string substring matching means
// the list can only over-include (precision is applied afterwards by parsing
// or by the per-file grep), never drop a file the fallback would have
// searched.
func tsCandidateFiles(ctx context.Context, sym, fileTypes string) []string {
	argv := []string{"-rlIF", "--color=never",
		"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build",
		"--exclude-dir=vendor", "--exclude-dir=target"}
	if fileTypes != "" {
		argv = append(argv, "--include="+fileTypes)
	}
	argv = append(argv, "--", sym, ".")
	out, _ := exec.CommandContext(ctx, "grep", argv...).Output()
	var files []string
	for _, ln := range strings.Split(string(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			files = append(files, ln)
		}
	}
	return files
}

// tsGrepFile runs the regex path against a single unsupported (or
// unparseable) file and strips comment-only matches, mirroring what the
// default build's project-wide grep + filterCodeMatches would have kept. -H
// forces the `path:line:content` shape grep only prints unprompted when it
// recurses.
func tsGrepFile(ctx context.Context, args ...string) string {
	argv := append([]string{"-H", "--color=never"}, args...)
	out, _ := exec.CommandContext(ctx, "grep", argv...).CombinedOutput()
	return filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 0)
}

// tsSplitGrepLine parses a `path:line:content` grep line back into a tsMatch
// so regex-sourced and AST-sourced results sort together.
func tsSplitGrepLine(ln string) tsMatch {
	parts := strings.SplitN(ln, ":", 3)
	if len(parts) == 3 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			return tsMatch{path: parts[0], line: n, content: parts[2]}
		}
	}
	return tsMatch{path: ln}
}

// tsDocRows returns the contiguous run of line-comment rows immediately above
// row, top to bottom. Only "//" and "#" lines attach — block doc comments
// (JSDoc and friends) are not picked up, and any blank or code line breaks
// the run, which is what keeps license headers at the top of a file from
// sticking to the first definition.
func tsDocRows(lines []string, row int) []int {
	var rows []int
	for r := row - 1; r >= 0; r-- {
		t := strings.TrimSpace(lines[r])
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") {
			rows = append([]int{r}, rows...)
			continue
		}
		break
	}
	return rows
}

// tsScanDefs parses displayPath (when its language is supported) and returns
// the definition lines for sym — with doc comments attached when withDocs is
// set (code_definition), without for find_symbol. ok=false means the caller
// should fall back to the per-file grep for this file.
func tsScanDefs(displayPath, sym string, withDocs bool) (matches []tsMatch, ok bool) {
	lang, ok := tslang.ForExt(filepath.Ext(displayPath))
	if !ok {
		return nil, false
	}
	tslang.Mu.Lock()
	defer tslang.Mu.Unlock()
	pf, ok := tsParseLocked(displayPath, lang)
	if !ok {
		return nil, false
	}
	seenRow := map[int]bool{}
	for _, q := range lang.DefQueries {
		defIdx, hasDef := q.CaptureIndexForName("def")
		nameIdx, hasName := q.CaptureIndexForName("name")
		if !hasDef || !hasName {
			continue
		}
		cur := tree_sitter.NewQueryCursor()
		ms := cur.Matches(q, pf.tree.RootNode(), pf.src)
		for m := ms.Next(); m != nil; m = ms.Next() {
			var defNode, nameNode *tree_sitter.Node
			for i := range m.Captures {
				c := &m.Captures[i]
				switch uint(c.Index) {
				case defIdx:
					n := c.Node
					defNode = &n
				case nameIdx:
					n := c.Node
					nameNode = &n
				}
			}
			if defNode == nil || nameNode == nil || nameNode.Utf8Text(pf.src) != sym {
				continue
			}
			row := int(defNode.StartPosition().Row)
			if row >= len(pf.lines) || seenRow[row] {
				continue
			}
			seenRow[row] = true
			if withDocs {
				for _, dr := range tsDocRows(pf.lines, row) {
					matches = append(matches, tsMatch{displayPath, dr + 1, pf.lines[dr]})
				}
			}
			matches = append(matches, tsMatch{displayPath, row + 1, pf.lines[row]})
		}
		cur.Close()
	}
	return matches, true
}

// tsScanRefs parses displayPath and returns one line per source row that
// references sym — identifier-kind leaves only, so comments and string
// literals are excluded by construction. ok=false routes the file to the
// per-file grep fallback.
func tsScanRefs(displayPath, sym string) (matches []tsMatch, ok bool) {
	lang, ok := tslang.ForExt(filepath.Ext(displayPath))
	if !ok {
		return nil, false
	}
	tslang.Mu.Lock()
	defer tslang.Mu.Unlock()
	pf, ok := tsParseLocked(displayPath, lang)
	if !ok {
		return nil, false
	}
	seenRow := map[int]bool{}
	var walk func(n *tree_sitter.Node)
	walk = func(n *tree_sitter.Node) {
		if lang.IdentKinds[n.Kind()] && n.Utf8Text(pf.src) == sym {
			row := int(n.StartPosition().Row)
			if row < len(pf.lines) && !seenRow[row] {
				seenRow[row] = true
				matches = append(matches, tsMatch{displayPath, row + 1, pf.lines[row]})
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(pf.tree.RootNode())
	return matches, true
}

// tsSortAndFormat orders matches by (path, line) — the regex path inherits
// grep's traversal order, which a merged AST+grep result set cannot reproduce,
// so the tag build sorts instead — and applies the cap with the same
// truncation footer filterCodeMatches emits.
func tsSortAndFormat(matches []tsMatch, limit int) string {
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].path != matches[j].path {
			return matches[i].path < matches[j].path
		}
		return matches[i].line < matches[j].line
	})
	truncated := 0
	if limit > 0 && len(matches) > limit {
		truncated = len(matches) - limit
		matches = matches[:limit]
	}
	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s:%d:%s", m.path, m.line, m.content)
	}
	out := b.String()
	if truncated > 0 {
		out += fmt.Sprintf("\n\n... and %d more (truncated at %d; narrow your search)", truncated, limit)
	}
	return out
}

// tsScanWorkspace implements find_symbol (withDocs=false) and code_definition
// (withDocs=true): grep-prefilter the workspace, AST-scan supported files,
// per-file grep the rest, merge, cap.
func tsScanWorkspace(ctx context.Context, sym, fileTypes, defPat string, withDocs bool, limit int) (string, bool) {
	var matches []tsMatch
	for _, f := range tsCandidateFiles(ctx, sym, fileTypes) {
		if ctx.Err() != nil {
			break
		}
		if m, ok := tsScanDefs(f, sym, withDocs); ok {
			matches = append(matches, m...)
			continue
		}
		if g := tsGrepFile(ctx, "-nE", "--", defPat, f); g != "" {
			for _, ln := range strings.Split(g, "\n") {
				matches = append(matches, tsSplitGrepLine(ln))
			}
		}
	}
	return tsSortAndFormat(matches, limit), true
}

// tsFindDefinitions is the tree-sitter body for code_definition: definition
// lines plus adjacent doc comments, capped like the regex path (50).
func tsFindDefinitions(ctx context.Context, sym, defPat string) (string, bool) {
	return tsScanWorkspace(ctx, sym, "", defPat, true, 50)
}

// tsFindSymbolLines is the tree-sitter body for find_symbol: definition lines
// only, capped like the regex path (100).
func tsFindSymbolLines(ctx context.Context, symbol, fileTypes, defPat string) (string, bool) {
	return tsScanWorkspace(ctx, symbol, fileTypes, defPat, false, 100)
}

// tsFindReferences is the tree-sitter body for code_references: identifier
// references in supported files (comments and strings excluded by
// construction), word-grep for the rest, capped like the regex path (50).
func tsFindReferences(ctx context.Context, sym string) (string, bool) {
	var matches []tsMatch
	for _, f := range tsCandidateFiles(ctx, sym, "") {
		if ctx.Err() != nil {
			break
		}
		if m, ok := tsScanRefs(f, sym); ok {
			matches = append(matches, m...)
			continue
		}
		if g := tsGrepFile(ctx, "-nwE", "--", sym, f); g != "" {
			for _, ln := range strings.Split(g, "\n") {
				matches = append(matches, tsSplitGrepLine(ln))
			}
		}
	}
	return tsSortAndFormat(matches, 50), true
}
