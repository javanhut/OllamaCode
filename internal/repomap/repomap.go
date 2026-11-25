// Package repomap builds a ranked, token-budgeted outline of a repository:
// the files that matter most and the top-level definitions in each.
//
// The design follows Aider's repo map (tree-sitter tags ranked with
// personalized PageRank and fit to a token budget), which Aider and Agentless
// both found to be a cheap, effective way to give a model the shape of a
// codebase without spending its context on file contents. The difference here
// is that it needs no parser: definitions come from per-language line
// patterns, so it works in the default CGO-free build. That costs precision
// (a definition inside a string literal can be picked up), which matters little
// for an outline whose job is "where should I look", not "what does this do".
package repomap

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/javanhut/ollama_code/internal/gitignore"
)

// Def is one top-level definition.
type Def struct {
	Name string
	Line int    // 1-based
	Text string // the trimmed source line, clipped
}

// File is one source file's definitions plus its PageRank score.
type File struct {
	Path  string // slash-separated, relative to the root
	Defs  []Def
	Score float64
}

// Options bounds a build.
type Options struct {
	MaxFiles     int      // files scanned at most (default 2000)
	MaxFileBytes int64    // larger files are skipped (default 256 KiB)
	Focus        []string // relative paths to bias the ranking toward (e.g. files the user mentioned)
}

var langPatterns = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`),
		regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`),
	},
	".py": {
		regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)`),
		regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`),
	},
	".js":  jsPatterns,
	".jsx": jsPatterns,
	".ts":  jsPatterns,
	".tsx": jsPatterns,
	".mjs": jsPatterns,
	".rs": {
		regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+([A-Za-z_]\w*)`),
		regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|type)\s+([A-Za-z_]\w*)`),
	},
	".java": {
		regexp.MustCompile(`^(?:public|protected|private)?\s*(?:abstract\s+|final\s+|static\s+)*(?:class|interface|enum|record)\s+([A-Za-z_]\w*)`),
	},
	".rb": {
		regexp.MustCompile(`^\s*(?:def|class|module)\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`),
	},
}

var jsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\*?\s+([A-Za-z_$][\w$]*)`),
	regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(?:class|interface|enum|type)\s+([A-Za-z_$][\w$]*)`),
	regexp.MustCompile(`^(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`),
}

var identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// Build scans root and ranks its files.
func Build(root string, opts Options) ([]File, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 2000
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 256 << 10
	}
	ignore := gitignore.NewMatcher(root)
	type scanned struct {
		file  File
		words map[string]int
	}
	var files []scanned
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || ignore.IsIgnored(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= opts.MaxFiles {
			return filepath.SkipAll
		}
		pats := langPatterns[strings.ToLower(filepath.Ext(path))]
		if pats == nil || ignore.IsIgnored(path) {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > opts.MaxFileBytes {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		s := scanned{file: File{Path: filepath.ToSlash(rel)}, words: map[string]int{}}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for line := 1; sc.Scan(); line++ {
			text := sc.Text()
			for _, w := range identifier.FindAllString(text, -1) {
				s.words[w]++
			}
			for _, re := range pats {
				if m := re.FindStringSubmatch(text); m != nil {
					s.file.Defs = append(s.file.Defs, Def{Name: m[1], Line: line, Text: clip(strings.TrimSpace(text), 100)})
					break
				}
			}
		}
		// A scan error (a line over 1 MiB, minified code) keeps the
		// definitions found so far: an outline of a partial file is still
		// an outline, and a generated file is rarely where the model should look.
		if sc.Err() != nil && len(s.file.Defs) == 0 {
			return nil
		}
		files = append(files, s)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Reference graph: file A -> file B when A mentions an identifier that B
	// defines. Common short names are ignored by the 3-char identifier floor;
	// names defined in many files (String, New, init) are down-weighted.
	definedIn := map[string][]int{}
	for i, s := range files {
		for _, d := range s.file.Defs {
			if !referenceable(d) {
				continue
			}
			definedIn[d.Name] = append(definedIn[d.Name], i)
		}
	}
	edges := make([]map[int]float64, len(files))
	for i, s := range files {
		edges[i] = map[int]float64{}
		for w, n := range s.words {
			targets := definedIn[w]
			if len(targets) == 0 || len(targets) > 8 {
				continue
			}
			for _, j := range targets {
				if j != i {
					edges[i][j] += float64(min(n, 5)) / float64(len(targets))
				}
			}
		}
	}

	focus := map[int]bool{}
	for _, p := range opts.Focus {
		p = filepath.ToSlash(filepath.Clean(p))
		for i, s := range files {
			if s.file.Path == p {
				focus[i] = true
			}
		}
	}
	scores := pageRank(edges, focus)
	out := make([]File, len(files))
	for i, s := range files {
		out[i] = s.file
		out[i].Score = scores[i]
		// Tests reference everything they exercise, so they rank as if they
		// were central; they are rarely where a change starts.
		if isTestFile(s.file.Path) && !focus[i] {
			out[i].Score *= 0.25
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Path < out[b].Path
	})
	return out, nil
}

// pageRank with an optional personalization set: random jumps land on the
// focus files only, which ranks what they depend on above the rest.
func pageRank(edges []map[int]float64, focus map[int]bool) []float64 {
	n := len(edges)
	if n == 0 {
		return nil
	}
	const damping, iterations = 0.85, 30
	jump := make([]float64, n)
	for i := range jump {
		if len(focus) == 0 {
			jump[i] = 1 / float64(n)
		} else if focus[i] {
			jump[i] = 1 / float64(len(focus))
		}
	}
	rank := append([]float64(nil), jump...)
	if len(focus) > 0 {
		for i := range rank {
			rank[i] = 1 / float64(n)
		}
	}
	outWeight := make([]float64, n)
	for i, e := range edges {
		for _, w := range e {
			outWeight[i] += w
		}
	}
	for range iterations {
		next := make([]float64, n)
		dangling := 0.0
		for i, e := range edges {
			if outWeight[i] == 0 {
				dangling += rank[i]
				continue
			}
			for j, w := range e {
				next[j] += damping * rank[i] * w / outWeight[i]
			}
		}
		for i := range next {
			next[i] += (1-damping)*jump[i] + damping*dangling*jump[i]
		}
		rank = next
	}
	return rank
}

// Render prints the highest-ranked files and their definitions until the
// token budget (estimated at ~4 chars per token) is spent. Files without
// definitions are skipped; each file lists at most maxDefsPerFile entries.
func Render(files []File, maxTokens int) string {
	const maxDefsPerFile = 12
	budget := maxTokens * 4
	var b strings.Builder
	b.WriteString("[REPO MAP — most-referenced files first, with their top-level definitions (file:line). A navigation aid: read the files before editing.]\n")
	shown := 0
	for _, f := range files {
		if len(f.Defs) == 0 {
			continue
		}
		var entry strings.Builder
		fmt.Fprintf(&entry, "\n%s\n", f.Path)
		for i, d := range f.Defs {
			if i == maxDefsPerFile {
				fmt.Fprintf(&entry, "  … %d more\n", len(f.Defs)-maxDefsPerFile)
				break
			}
			fmt.Fprintf(&entry, "  %d: %s\n", d.Line, d.Text)
		}
		if b.Len()+entry.Len() > budget && shown > 0 {
			remaining := 0
			for _, g := range files {
				if len(g.Defs) > 0 {
					remaining++
				}
			}
			fmt.Fprintf(&b, "\n(%d more files omitted to fit the budget; narrow with focus paths)\n", remaining-shown)
			break
		}
		b.WriteString(entry.String())
		shown++
	}
	if shown == 0 {
		return ""
	}
	return b.String()
}

func isTestFile(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/")
}

// goMethod matches a Go definition with a receiver.
var goMethod = regexp.MustCompile(`^func\s*\(`)

// referenceable reports whether a definition's name is specific enough that a
// bare mention elsewhere is evidence of a dependency. Methods are called as
// x.Name and collide across types and with builtins (a method named append,
// run, or close would otherwise "receive" every call to those); short
// lowercase names (get, run, init, main) are too generic to mean anything.
func referenceable(d Def) bool {
	if goMethod.MatchString(d.Text) {
		return false
	}
	if len(d.Name) < 5 && strings.ToLower(d.Name) == d.Name {
		return false
	}
	return true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
