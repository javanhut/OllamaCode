//go:build treesitter

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeSitterParseErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.py")
	if err := os.WriteFile(bad, []byte("def f(:\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.py")
	if err := os.WriteFile(good, []byte("def f():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errs := TreeSitterParseErrors([]string{bad, good, filepath.Join(dir, "notes.txt")})
	if len(errs) == 0 {
		t.Fatal("expected syntax errors for bad.py")
	}
	if !strings.Contains(errs[0], "bad.py:1") {
		t.Fatalf("diagnostic should locate the error: %q", errs[0])
	}
	for _, e := range errs {
		if strings.Contains(e, "good.py") {
			t.Fatalf("clean file reported as broken: %q", e)
		}
	}
}

func TestTreeSitterParseErrorsClean(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.py")
	if err := os.WriteFile(good, []byte("def f():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if errs := TreeSitterParseErrors([]string{good}); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}
