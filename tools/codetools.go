package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/javanhut/ollama_code/internal/semantic"
)

func CodeDefinitionTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_definition",
			Description: "Find where a symbol is defined. Give it a file path and line number, and it extracts the symbol name at that position then searches the project for its definition. Works for Go, Rust, Python, C/C++, JS/TS, Zig, and more.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol is used (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			// Read the file and extract the symbol name at the given line
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}
			targetLine := lines[a.Line-1]

			// Extract likely symbol: a word that starts with letter/underscore
			re := regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
			symbols := re.FindAllString(targetLine, -1)
			if len(symbols) == 0 {
				return "", fmt.Errorf("no symbol found on line %d", a.Line)
			}

			// Use the last non-keyword symbol (most likely the interesting one)
			keywords := map[string]bool{"func": true, "fn": true, "def": true, "class": true,
				"struct": true, "type": true, "var": true, "const": true, "let": true,
				"return": true, "if": true, "for": true, "import": true, "pub": true}
			sym := symbols[len(symbols)-1]
			for i := len(symbols) - 1; i >= 0; i-- {
				if !keywords[symbols[i]] {
					sym = symbols[i]
					break
				}
			}

			// Search for definition of this symbol
			pat := fmt.Sprintf(
				`(^|[[:space:]])(func|fn|class|struct|trait|enum|impl|interface|type|var|const|def)[[:space:]]+.*%s([[:space:]]|$)`,
				regexp.QuoteMeta(sym),
			)
			cmd := exec.CommandContext(ctx, "grep", "-rnE", "--color=never",
				"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build",
				"--exclude-dir=vendor", "--exclude-dir=target", pat, ".")
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 50)
			if text == "" {
				return fmt.Sprintf("definition of %q not found in project (found on line %d of %s)", sym, a.Line, a.Path), nil
			}
			return fmt.Sprintf("definition(s) for %q (referenced at %s:%d):\n%s",
				sym, a.Path, a.Line, text), nil
		},
	}
}

func CodeReferencesTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_references",
			Description: "Find all usages of a symbol across the project. Give it a file and line number; it extracts the symbol name and greps for all references (excluding comments). Use this to understand how a function or type is used before modifying it.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol appears (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}
			targetLine := lines[a.Line-1]

			re := regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
			symbols := re.FindAllString(targetLine, -1)
			if len(symbols) == 0 {
				return "", fmt.Errorf("no symbol found on line %d", a.Line)
			}

			// Filter out keywords
			keywords := map[string]bool{"func": true, "fn": true, "def": true, "class": true,
				"struct": true, "type": true, "var": true, "const": true, "let": true,
				"return": true, "if": true, "for": true, "import": true, "pub": true,
				"package": true, "else": true, "match": true, "switch": true, "case": true}
			sym := symbols[len(symbols)-1]
			for i := len(symbols) - 1; i >= 0; i-- {
				if !keywords[symbols[i]] {
					sym = symbols[i]
					break
				}
			}

			// Grep for all occurrences of this word (word-boundary match via -w)
			cmd := exec.CommandContext(ctx, "grep", "-rnwE", "--color=never",
				"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build",
				"--exclude-dir=vendor", "--exclude-dir=target", sym, ".")
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 50)
			if text == "" {
				return fmt.Sprintf("no references to %q found in project", sym), nil
			}
			return fmt.Sprintf("references to %q (comments excluded):\n%s", sym, text), nil
		},
	}
}

func CodeHoverTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_hover",
			Description: "Show the definition context surrounding a symbol. Give it a file and line number; it finds the symbol at that line and returns its full definition block — function signature, struct fields, class definition, etc. Use this to quickly understand what something is without reading the whole file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol appears (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}

			// Find the enclosing definition block
			// Look backwards for the start (func, fn, def, class, struct, type, etc.)
			start := a.Line - 1
			defRe := regexp.MustCompile(`^\s*(func|fn|def|class|struct|enum|trait|impl|interface|type|pub\s+(fn|struct|enum|trait|type))\s`)
			for start > 0 && !defRe.MatchString(lines[start]) {
				start--
			}

			// Forward to find the end (closing brace at same indent, or empty line for Python)
			end := a.Line - 1
			baseIndent := len(lines[start]) - len(strings.TrimLeft(lines[start], " \t"))
			braceCount := 0
			inBlock := false
			hasBraces := strings.Contains(lines[start], "{")
			isPython := strings.HasPrefix(strings.TrimSpace(lines[start]), "def ") || strings.HasPrefix(strings.TrimSpace(lines[start]), "class ")

			if isPython {
				for end < len(lines)-1 {
					end++
					if strings.TrimSpace(lines[end]) == "" {
						continue
					}
					indent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
					if indent <= baseIndent && end > a.Line-1 {
						end--
						break
					}
				}
			} else if hasBraces {
				for end < len(lines) {
					line := lines[end]
					for _, c := range line {
						if c == '{' {
							braceCount++
							inBlock = true
						} else if c == '}' {
							braceCount--
						}
					}
					if inBlock && braceCount == 0 {
						break
					}
					end++
				}
				if end >= len(lines) {
					end = len(lines) - 1
				}
			} else {
				// Simple: just grab 5 lines of context
				end = start + 5
				if end >= len(lines) {
					end = len(lines) - 1
				}
			}

			if start > end {
				end = start
			}

			var buf strings.Builder
			for i := start; i <= end; i++ {
				fmt.Fprintf(&buf, "%d\t%s\n", i+1, lines[i])
			}
			return fmt.Sprintf("definition context for %s, lines %d-%d:\n%s",
				a.Path, start+1, end+1, strings.TrimRight(buf.String(), "\n")), nil
		},
	}
}

func FindSymbolTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "find_symbol",
			Description: "Search for a symbol definition across the project. Matches Go func/method/type/var/const, Rust fn/struct/trait/enum/impl/type, Python class/def, C/C++ functions, Java/TS classes, and more. Use this to locate where things are defined.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"symbol":     {Type: "string", Description: "The name of the symbol to find."},
					"file_types": {Type: "string", Description: "Optional comma-separated extensions to filter by (e.g. '.go,.rs,.py')."},
				},
				Required: []string{"symbol"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Symbol    string `json:"symbol"`
				FileTypes string `json:"file_types"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Symbol == "" {
				return "", fmt.Errorf("symbol is required")
			}

			// Broad pattern covering many languages:
			// Go: func, func (receiver), type, var, const
			// Rust: fn, struct, trait, enum, impl, type, const, static
			// Python: class, def, async def, @decorators
			// C/C++/Java/TS/JS: function declarations, class, interface, enum
			// Zig: fn, const, var, struct, enum, union
			sym := regexp.QuoteMeta(a.Symbol)
			pattern := fmt.Sprintf(
				`(^|[[:space:]])(func|fn|class|struct|trait|enum|impl|interface|type|var|const|def|let|static|async[[:space:]]+def|union|module|pub)[[:space:]]+.*%s|^[[:space:]]*%s[[:space:]]*:=|^[[:space:]]*%s[[:space:]]*=`,
				sym, sym, sym,
			)
			argv := []string{"-rnE", "--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build", "--exclude-dir=vendor", "--exclude-dir=target", pattern}
			if a.FileTypes != "" {
				argv = append(argv, "--include="+a.FileTypes)
			}
			argv = append(argv, ".")
			cmd := exec.CommandContext(ctx, "grep", argv...)
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 100)
			if text == "" {
				return fmt.Sprintf("symbol %q not found in project", a.Symbol), nil
			}
			return text, nil
		},
	}
}

func CodeIndexTool(embedder Embedder) Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_index",
			Description: "Build or rebuild a semantic embedding index for the current project. The index enables natural-language code search via semantic_search. You can force a rebuild with force_rebuild=true.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"model":         {Type: "string", Description: "Embedding model to use (default: nomic-embed-text)."},
					"force_rebuild": {Type: "boolean", Description: "Rebuild even if a cached index exists."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Model        string `json:"model"`
				ForceRebuild bool   `json:"force_rebuild"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.Model == "" {
				a.Model = "nomic-embed-text"
			}
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			if !a.ForceRebuild {
				if _, err := semantic.LoadIndex(cwd); err == nil {
					return "Semantic index already exists. Use force_rebuild=true to rebuild.", nil
				}
			}
			idx, err := semantic.BuildIndex(cwd, a.Model, func(inputs []string) ([][]float32, error) {
				return embedder.Embed(a.Model, inputs)
			})
			if err != nil {
				return "", fmt.Errorf("index build failed: %w", err)
			}
			if err := semantic.SaveIndex(idx); err != nil {
				return "", fmt.Errorf("save index failed: %w", err)
			}
			return fmt.Sprintf("Indexed %d chunks from project %s using model %s", len(idx.Chunks), cwd, a.Model), nil
		},
	}
}

func SemanticSearchTool(embedder Embedder) Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "semantic_search",
			Description: "Search the project with natural language using a pre-built semantic embedding index. Returns the top_k most relevant code snippets.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"query": {Type: "string", Description: "Natural language query."},
					"top_k": {Type: "number", Description: "Number of results to return. Default 5."},
					"model": {Type: "string", Description: "Embedding model used for the index (default: nomic-embed-text). Must match the model passed to code_index."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
				TopK  int    `json:"top_k"`
				Model string `json:"model"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.TopK <= 0 {
				a.TopK = 5
			}
			if a.Model == "" {
				a.Model = "nomic-embed-text"
			}
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			idx, err := semantic.LoadIndex(cwd)
			if err != nil {
				return "No semantic index found. Run code_index first.", nil
			}
			results, err := idx.Search(a.Query, func(q string) ([]float32, error) {
				embs, err := embedder.Embed(a.Model, []string{q})
				if err != nil {
					return nil, err
				}
				if len(embs) == 0 {
					return nil, fmt.Errorf("empty embedding response")
				}
				return embs[0], nil
			}, a.TopK)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for i, r := range results {
				fmt.Fprintf(&b, "%d. %s:%d-%d (score: %.3f)\n%s\n\n", i+1, r.Path, r.StartLine, r.EndLine, r.Score, r.Text)
			}
			return strings.TrimSpace(b.String()), nil
		},
	}
}
