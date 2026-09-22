package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWalksRootToCwdBroadToSpecific(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := t.TempDir()
	write(t, filepath.Join(global, "AGENTS.md"), "global rule")
	write(t, filepath.Join(root, "AGENTS.md"), "root rule")
	write(t, filepath.Join(root, "CLAUDE.md"), "claude rule — shadowed by AGENTS.md in the same dir")
	write(t, filepath.Join(root, "pkg", "CLAUDE.md"), "pkg rule")
	write(t, filepath.Join(root, "pkg", "api", "OLLAMA.md"), "api rule")
	write(t, filepath.Join(root, "pkg", "api", "deep", "AGENTS.md"), "deeper than cwd — lazy only")
	cwd := filepath.Join(root, "pkg", "api")

	set := Load(Options{Cwd: cwd, GlobalDir: global})
	var bodies []string
	for _, f := range set.Files {
		bodies = append(bodies, f.Content)
	}
	want := []string{"global rule", "root rule", "pkg rule", "api rule"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("loaded %q, want %q", bodies, want)
	}
	if set.Root != root {
		t.Fatalf("root = %q, want %q", set.Root, root)
	}
	out := set.Render()
	if !strings.Contains(out, "# Project instructions") || !strings.Contains(out, "Instructions from: "+filepath.Join(root, "AGENTS.md")) {
		t.Fatalf("render missing headers:\n%s", out)
	}

	tr := NewTracker(cwd, set)
	lazy := tr.ForPath(filepath.Join(cwd, "deep", "x.go"))
	if !strings.Contains(lazy, "deeper than cwd") {
		t.Fatalf("tracker did not attach nested file: %q", lazy)
	}
	if again := tr.ForPath("deep/y.go"); again != "" {
		t.Fatalf("nested file attached twice: %q", again)
	}
	if outside := tr.ForPath(filepath.Join(root, "AGENTS.md")); outside != "" {
		t.Fatalf("paths above cwd must not attach: %q", outside)
	}
}

func TestLoadDedupesIdenticalContentAndExtras(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "same")
	write(t, filepath.Join(root, "docs", "rules.md"), "same")
	write(t, filepath.Join(root, "docs", "style.md"), "style")
	set := Load(Options{Cwd: root, NoGlobal: true, Extra: []string{"docs/rules.md", "docs/style.md", "missing.md"}})
	if len(set.Files) != 2 || set.Files[0].Content != "same" || set.Files[1].Content != "style" {
		t.Fatalf("unexpected files: %+v", set.Files)
	}
}

func TestBudgetDropsBroadThenTruncates(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), "ref")
	write(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", 60))
	write(t, filepath.Join(root, "sub", "AGENTS.md"), strings.Repeat("b", 80))
	set := Load(Options{Cwd: filepath.Join(root, "sub"), NoGlobal: true, MaxBytes: 50})
	if len(set.Files) != 1 || !strings.HasPrefix(set.Files[0].Content, strings.Repeat("b", 50)) ||
		!strings.Contains(set.Files[0].Content, "truncated") {
		t.Fatalf("unexpected fit: %+v", set.Files)
	}
	if len(set.Dropped) != 2 {
		t.Fatalf("dropped = %v", set.Dropped)
	}
}

func TestNoFilesRendersEmpty(t *testing.T) {
	if got := Load(Options{Cwd: t.TempDir(), NoGlobal: true}).Render(); got != "" {
		t.Fatalf("expected empty render, got %q", got)
	}
}
