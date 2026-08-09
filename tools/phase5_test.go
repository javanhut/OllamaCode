package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestApplyEdit_FuzzyAccept(t *testing.T) {
	content := "func process(items []int) int {\n\ttotal := 0\n\tfor _, x := range items {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	// One-character typo: not an exact or whitespace match, but unambiguously
	// the loop line. Should fuzzy-match and apply.
	old := "for _, x := range itms {"
	got, count, tier, err := applyEdit(content, old, "for _, x := range items { // fixed", false)
	if err != nil {
		t.Fatalf("expected fuzzy accept, got error: %v", err)
	}
	if tier != 3 || count != 1 {
		t.Fatalf("expected tier 3 single match, got tier=%d count=%d", tier, count)
	}
	if !strings.Contains(got, "// fixed") || strings.Contains(got, "itms") {
		t.Fatalf("fuzzy edit not applied correctly:\n%s", got)
	}
}

func TestApplyEdit_FuzzyRejectAmbiguous(t *testing.T) {
	// Two near-identical regions: no clear winner -> refuse.
	content := "alpha one\nbeta two\n\n\nalpha one\nbeta two\n"
	_, _, tier, err := applyEdit(content, "alpha onee\nbeta two", "x\ny", false)
	if err == nil {
		t.Fatal("expected refusal for ambiguous fuzzy match")
	}
	if tier != 3 {
		t.Fatalf("expected tier 3, got %d", tier)
	}
}

func TestApplyEdit_FuzzyRejectLowScore(t *testing.T) {
	content := "the quick brown fox\njumps over\nthe lazy dog\n"
	_, _, _, err := applyEdit(content, "completely unrelated content xyz", "x", false)
	if err == nil {
		t.Fatal("expected refusal when nothing is similar")
	}
	if !strings.Contains(err.Error(), "Closest current text") {
		t.Fatalf("refusal should include the closest region, got: %v", err)
	}
}

func TestVerifyBytes(t *testing.T) {
	if err := verifyBytes("x.go", []byte("package x\nfunc A() {}\n")); err != nil {
		t.Errorf("valid go should pass: %v", err)
	}
	if err := verifyBytes("x.go", []byte("package x\nfunc A( {\n")); err == nil {
		t.Error("broken go should fail")
	}
	if err := verifyBytes("x.json", []byte(`{"a":1}`)); err != nil {
		t.Errorf("valid json should pass: %v", err)
	}
	if err := verifyBytes("x.json", []byte(`{bad`)); err == nil {
		t.Error("broken json should fail")
	}
	if err := verifyBytes("notes.txt", []byte("anything at all (")); err != nil {
		t.Errorf("unknown extension should pass: %v", err)
	}
}

func TestWriteFileVerifyGate(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	ctx := context.Background()
	write := WriteFileTool()

	// Broken syntax in a gated extension is rejected with a retryable error,
	// and nothing is written — the file must not even be created.
	_, err := write.Handler(ctx, jailArgs(t, map[string]string{
		"path": "broken.go", "content": "package x\nfunc A( {\n",
	}))
	if err == nil {
		t.Fatal("broken go write should be rejected")
	}
	if !strings.Contains(err.Error(), "syntax error") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("rejection should explain the syntax error and invite a retry, got: %v", err)
	}
	if _, statErr := os.Stat("broken.go"); !os.IsNotExist(statErr) {
		t.Fatal("rejected write must not create the file")
	}

	// Same gate for JSON.
	if _, err := write.Handler(ctx, jailArgs(t, map[string]string{
		"path": "broken.json", "content": `{bad`,
	})); err == nil {
		t.Fatal("broken json write should be rejected")
	}

	// Valid content writes fine.
	if _, err := write.Handler(ctx, jailArgs(t, map[string]string{
		"path": "ok.go", "content": "package x\nfunc A() {}\n",
	})); err != nil {
		t.Fatalf("valid go write: %v", err)
	}

	// Unlike edit_file, write_file supplies the whole file, so a rewrite that
	// breaks syntax is rejected even though the existing file parses.
	if _, err := write.Handler(ctx, jailArgs(t, map[string]string{
		"path": "ok.go", "content": "package x\nfunc B( {\n",
	})); err == nil {
		t.Fatal("rewriting a valid file with broken go should be rejected")
	}
	if data, _ := os.ReadFile("ok.go"); !strings.Contains(string(data), "func A()") {
		t.Fatalf("rejected rewrite must leave the file untouched, got:\n%s", data)
	}

	// Unknown extensions are never gated.
	if _, err := write.Handler(ctx, jailArgs(t, map[string]string{
		"path": "notes.txt", "content": "anything ( at all",
	})); err != nil {
		t.Fatalf("unknown extension must bypass the gate: %v", err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	diff := unifiedDiff("a\nb\nc\n", "a\nB\nc\n", "f.txt")
	if !strings.Contains(diff, "-b") || !strings.Contains(diff, "+B") {
		t.Fatalf("diff should show the changed line:\n%s", diff)
	}
	if unifiedDiff("same\n", "same\n", "f.txt") != "" {
		t.Fatal("identical content should produce empty diff")
	}
}

func TestUnifiedDiffHunkHeader(t *testing.T) {
	// Change line 5 of a 9-line file: hunk starts at line 2 (3 lines of context).
	old := "1\n2\n3\n4\nx\n6\n7\n8\n9\n"
	new := "1\n2\n3\n4\ny\n6\n7\n8\n9\n"
	diff := unifiedDiff(old, new, "f.txt")
	if !strings.Contains(diff, "@@ -2,7 +2,7 @@") {
		t.Fatalf("want hunk header @@ -2,7 +2,7 @@, got:\n%s", diff)
	}
}
