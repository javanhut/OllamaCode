//go:build treesitter

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end tests for the tree-sitter precision path (build tag
// treesitter). Each test builds a scratch workspace on disk and drives the
// real tool handlers, which grep-prefilter from "." — hence the Chdir. The
// regex path is exercised everywhere else (untagged tests); here we assert
// the behavioral differences parsing buys.

func tsWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tsCall(t *testing.T, tool Tool, args string) string {
	t.Helper()
	out, err := tool.Handler(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return out
}

func TestTreeSitterFindSymbol_GoPrecision(t *testing.T) {
	dir := t.TempDir()
	// A method whose name is immediately followed by "(" — the regex
	// definition pattern requires whitespace or EOL after the name and
	// misses this; the AST does not. Also a comment mention that must not
	// surface as a definition.
	tsWriteFile(t, dir, "main.go", `package main

// Hello greets the world.
func Hello() { println("hi") }

type Server struct{}

func (s *Server) Hello2() {}

// Hello is mentioned in a comment only.
func main() { Hello() }
`)
	t.Chdir(dir)

	out := tsCall(t, FindSymbolTool(), `{"symbol":"Hello"}`)
	if !strings.Contains(out, "main.go:4:func Hello() {") {
		t.Fatalf("expected func definition line, got:\n%s", out)
	}
	if strings.Contains(out, "Hello is mentioned") {
		t.Fatalf("comment mention must not be a definition:\n%s", out)
	}
	if strings.Contains(out, "Hello2") {
		t.Fatalf("substring match on Hello2 must not happen for symbol Hello:\n%s", out)
	}
}

func TestTreeSitterFindSymbol_MultiNameDeclarations(t *testing.T) {
	dir := t.TempDir()
	// Repeated name fields (`var a, b int`, `x, y := ...`) must each produce
	// a match — a capture on only the first name would silently lose the
	// rest of the declaration.
	tsWriteFile(t, dir, "multi.go", `package main

var alpha, beta int

func use() {
	x, y := 1, 2
	_, _ = x, y
}
`)
	t.Chdir(dir)

	if out := tsCall(t, FindSymbolTool(), `{"symbol":"beta"}`); !strings.Contains(out, "multi.go:3:var alpha, beta int") {
		t.Fatalf("second name in var spec must be found, got:\n%s", out)
	}
	if out := tsCall(t, FindSymbolTool(), `{"symbol":"y"}`); !strings.Contains(out, "multi.go:6:\tx, y := 1, 2") {
		t.Fatalf("second name in short var decl must be found, got:\n%s", out)
	}
}

func TestTreeSitterCodeDefinition_DocCommentAttached(t *testing.T) {
	dir := t.TempDir()
	tsWriteFile(t, dir, "main.go", `package main

// Hello greets the world.
// Callers should pass a name.
func Hello() { println("hi") }

func main() { Hello() }
`)
	t.Chdir(dir)

	out := tsCall(t, CodeDefinitionTool(), `{"path":"main.go","line":7}`)
	if !strings.Contains(out, `definition(s) for "Hello"`) {
		t.Fatalf("expected definition header, got:\n%s", out)
	}
	if !strings.Contains(out, "main.go:5:func Hello() {") {
		t.Fatalf("expected definition line, got:\n%s", out)
	}
	if !strings.Contains(out, "// Hello greets the world.") || !strings.Contains(out, "// Callers should pass a name.") {
		t.Fatalf("expected adjacent doc comment lines, got:\n%s", out)
	}
}

func TestTreeSitterCodeReferences_ExcludesCommentsAndStrings(t *testing.T) {
	dir := t.TempDir()
	tsWriteFile(t, dir, "main.go", `package main

func Hello() {}

func main() {
	// Hello is called here (comment mention).
	msg := "Hello world"
	_ = msg
	Hello()
}
`)
	t.Chdir(dir)

	out := tsCall(t, CodeReferencesTool(), `{"path":"main.go","line":9}`)
	if !strings.Contains(out, "main.go:9:\tHello()") {
		t.Fatalf("expected the real call, got:\n%s", out)
	}
	if !strings.Contains(out, "main.go:3:func Hello()") {
		t.Fatalf("the definition name is itself a reference (grep parity), got:\n%s", out)
	}
	if strings.Contains(out, "comment mention") {
		t.Fatalf("comment mention must be excluded:\n%s", out)
	}
	if strings.Contains(out, `"Hello world"`) {
		t.Fatalf("string literal must be excluded:\n%s", out)
	}
}

func TestTreeSitterFindSymbol_MultiLanguage(t *testing.T) {
	dir := t.TempDir()
	tsWriteFile(t, dir, "app.py", `# module

def Hello():
    pass
`)
	tsWriteFile(t, dir, "web.ts", `class Hello {
    greet() {}
}

interface Helloable {
    greet(): void
}
`)
	t.Chdir(dir)

	out := tsCall(t, FindSymbolTool(), `{"symbol":"Hello"}`)
	if !strings.Contains(out, "app.py:3:def Hello():") {
		t.Fatalf("expected python def, got:\n%s", out)
	}
	if !strings.Contains(out, "web.ts:1:class Hello {") {
		t.Fatalf("expected typescript class, got:\n%s", out)
	}
	// file_types narrowing is honored by the prefilter.
	narrow := tsCall(t, FindSymbolTool(), `{"symbol":"Hello","file_types":"*.py"}`)
	if strings.Contains(narrow, "web.ts") || !strings.Contains(narrow, "app.py") {
		t.Fatalf("file_types filter not honored, got:\n%s", narrow)
	}
}

func TestTreeSitterFindSymbol_UnsupportedLanguageFallsBack(t *testing.T) {
	dir := t.TempDir()
	// C is not a compiled-in grammar; its files must still be searched via
	// the same regex the default build uses ("static" is one of the keyword
	// alternatives in the find_symbol pattern, so the fallback can see it).
	tsWriteFile(t, dir, "hello.c", `static int counter = 0;

int bumpcounter(void) {
    return ++counter;
}
`)
	t.Chdir(dir)

	out := tsCall(t, FindSymbolTool(), `{"symbol":"counter"}`)
	if !strings.Contains(out, "hello.c:1:static int counter = 0;") {
		t.Fatalf("unsupported-language file must be searched by regex fallback, got:\n%s", out)
	}
}

func TestTreeSitterNotFoundMessage(t *testing.T) {
	dir := t.TempDir()
	tsWriteFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	t.Chdir(dir)

	out := tsCall(t, FindSymbolTool(), `{"symbol":"Nonexistent"}`)
	if !strings.Contains(out, `symbol "Nonexistent" not found in project`) {
		t.Fatalf("expected not-found message, got:\n%s", out)
	}
}

func TestTreeSitterCacheReuseAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	tsWriteFile(t, dir, "main.go", `package main

func Hello() {}

func main() { Hello() }
`)
	t.Chdir(dir)

	// Two different tools parse the same file; the second must hit the
	// (size, mtime)-validated cache and produce identical content.
	first := tsCall(t, FindSymbolTool(), `{"symbol":"Hello"}`)
	second := tsCall(t, CodeReferencesTool(), `{"path":"main.go","line":5}`)
	if !strings.Contains(first, "main.go:3:func Hello()") {
		t.Fatalf("first call wrong:\n%s", first)
	}
	if !strings.Contains(second, "main.go:5:func main() { Hello() }") {
		t.Fatalf("second call wrong:\n%s", second)
	}
}
