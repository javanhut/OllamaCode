package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaceholderLine(t *testing.T) {
	yes := []string{
		"// ... existing code ...",
		"    # ... rest of the file",
		"/* ... */",
		"// ...",
		"// …",
		"<!-- ... -->",
		"// rest of the file unchanged",
		"  // Rest of the implementation remains unchanged.",
		"# existing code here",
		"-- other functions",
		"// unchanged",
		"...",
	}
	no := []string{
		"// Other methods are defined in fs.go",
		"// TODO: handle errors...",
		"// Wait for it...",
		"// see https://example.com/...",
		"#!/bin/sh",
		"# etc.",
		"x := call(args...)",
		"// The rest of this package assumes UTF-8.",
		"return nil",
	}
	for _, l := range yes {
		if !placeholderLine(l, false) {
			t.Errorf("placeholderLine(%q) = false, want true", l)
		}
	}
	for _, l := range no {
		if placeholderLine(l, false) {
			t.Errorf("placeholderLine(%q) = true, want false", l)
		}
	}
	if placeholderLine("    ...", true) {
		t.Error("a bare ... in Python is Ellipsis, not a placeholder")
	}
}

func TestFindPlaceholderAllowsLinesAlreadyInFile(t *testing.T) {
	orig := "def f():\n    # ... existing code ...\n    pass\n"
	if p := findPlaceholder("a.go", "// x\n# ... existing code ...\n", orig); p != "" {
		t.Fatalf("a line already in the file was flagged: %q", p)
	}
	if p := findPlaceholder("README.md", "# Other functions\n...", ""); p != "" {
		t.Fatalf("prose was flagged: %q", p)
	}
	if p := findPlaceholder("a.go", "func a() {}\n// ... rest unchanged\n", "func a() {}\n"); p != "// ... rest unchanged" {
		t.Fatalf("p = %q", p)
	}
}

func placeholderWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	return root
}

func TestEditRejectsPlaceholderNewString(t *testing.T) {
	root := placeholderWorkspace(t)
	p := filepath.Join(root, "a.go")
	os.WriteFile(p, []byte("package a\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\n\treturn 2\n}\n"), 0o644)

	_, _, _, _, err := resolveEdit(editArgs{Path: p, OldString: "func A() int {\n\treturn 1\n}",
		NewString: "func A() int {\n\treturn 3\n}\n\n// ... rest of the file unchanged"})
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("err = %v, want a placeholder rejection", err)
	}
}

func TestWriteFileRejectsPlaceholderOverExistingFile(t *testing.T) {
	root := placeholderWorkspace(t)
	p := filepath.Join(root, "a.go")
	orig := "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"
	os.WriteFile(p, []byte(orig), 0o644)
	args, _ := json.Marshal(map[string]string{"path": p, "content": "package a\n\nfunc A() int { return 3 }\n\n// ... existing code ...\n"})

	if _, err := WriteFileTool().Handler(context.Background(), args); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("err = %v, want a placeholder rejection", err)
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Fatal("the file was changed despite the rejection")
	}
}

func TestEditFallsBackToWholeFileOnSmallFile(t *testing.T) {
	root := placeholderWorkspace(t)
	p := filepath.Join(root, "a.go")
	content := "package a\n\nfunc A() int {\n\treturn 1\n}\n"
	os.WriteFile(p, []byte(content), 0o644)
	args, _ := json.Marshal(map[string]string{"path": p, "old_string": "func Missing() {\n\tpanic(\"nope\")\n}", "new_string": "x"})

	out, err := EditFileTool().Handler(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("err = %v, want the whole-file suggestion", err)
	}
	if out != content {
		t.Fatalf("output = %q, want the file's current contents", out)
	}
}

func TestEditFallbackSkipsLargeFilesAndAmbiguity(t *testing.T) {
	root := placeholderWorkspace(t)
	big := filepath.Join(root, "big.go")
	os.WriteFile(big, []byte("package a\n"+strings.Repeat("var _ = 1\n", wholeFileMaxLines+10)), 0o644)
	args, _ := json.Marshal(map[string]string{"path": big, "old_string": "func Missing() {}", "new_string": "x"})
	if out, err := EditFileTool().Handler(context.Background(), args); err == nil || out != "" || strings.Contains(err.Error(), "write_file") {
		t.Fatalf("large file: out=%d bytes err=%v", len(out), err)
	}

	dup := filepath.Join(root, "dup.go")
	os.WriteFile(dup, []byte("package a\nvar x = 1\nvar x = 1\n"), 0o644)
	args, _ = json.Marshal(map[string]string{"path": dup, "old_string": "var x = 1", "new_string": "var y = 2"})
	_, err := EditFileTool().Handler(context.Background(), args)
	var nm *noMatchError
	if err == nil || errors.As(err, &nm) {
		t.Fatalf("an ambiguous match is not a no-match: err=%v", err)
	}
}
