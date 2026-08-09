package verification

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeStaticcheck installs an executable named staticcheck in a temp dir on
// PATH, so tests never depend on the real binary being installed.
func fakeStaticcheck(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "staticcheck")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func writeGoMod(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLintCommandSilentWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	root := t.TempDir()
	writeGoMod(t, root)
	if _, ok := LintCommand(root, []string{"main.go"}); ok {
		t.Fatal("expected no lint command when staticcheck is not installed")
	}
	if got := Lint(context.Background(), root, []string{"main.go"}); got != "" {
		t.Fatalf("expected silent lint without binary, got %q", got)
	}
}

func TestLintCommandScopesToChangedPackages(t *testing.T) {
	fakeStaticcheck(t, "#!/bin/sh\nexit 0\n")
	root := t.TempDir()
	writeGoMod(t, root)
	cmd, ok := LintCommand(root, []string{"internal/a/a.go", "main.go", "README.md"})
	if !ok {
		t.Fatal("expected a lint command for changed Go files")
	}
	if cmd != "staticcheck . ./internal/a" {
		t.Fatalf("unexpected scoping: %q", cmd)
	}
	if _, ok := LintCommand(root, []string{"README.md"}); ok {
		t.Fatal("expected no lint command when no Go files changed")
	}
}

func TestLintReportsOnlyChangedFiles(t *testing.T) {
	fakeStaticcheck(t, "#!/bin/sh\n"+
		"echo 'main.go:12:5: this value of err is never used (SA4006)'\n"+
		"echo 'other.go:3:1: package comment should be of the form (ST1000)'\n"+
		"exit 1\n")
	root := t.TempDir()
	writeGoMod(t, root)
	got := Lint(context.Background(), root, []string{"main.go"})
	if !strings.Contains(got, "main.go:12:5") {
		t.Fatalf("missing diagnostic for changed file: %q", got)
	}
	if strings.Contains(got, "other.go") {
		t.Fatalf("diagnostics for untouched files must be filtered out: %q", got)
	}
}

func TestLintSilentOnCleanRun(t *testing.T) {
	fakeStaticcheck(t, "#!/bin/sh\nexit 0\n")
	root := t.TempDir()
	writeGoMod(t, root)
	if got := Lint(context.Background(), root, []string{"main.go"}); got != "" {
		t.Fatalf("expected no diagnostics from a clean run, got %q", got)
	}
}

func TestLintDropsNonDiagnosticNoise(t *testing.T) {
	fakeStaticcheck(t, "#!/bin/sh\necho \"couldn't load packages: whatever\"\nexit 1\n")
	root := t.TempDir()
	writeGoMod(t, root)
	if got := Lint(context.Background(), root, []string{"main.go"}); got != "" {
		t.Fatalf("loader errors are not diagnostics, expected silence, got %q", got)
	}
}

func TestFormatLintDiagnosticsCapsCount(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		b.WriteString("main.go:1:1: finding\n")
	}
	got := FormatLintDiagnostics(b.String(), []string{"main.go"})
	lines := strings.Split(got, "\n")
	if len(lines) != maxLintDiagnostics+1 { // capped lines + omission note
		t.Fatalf("expected %d lines, got %d", maxLintDiagnostics+1, len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "omitted") {
		t.Fatalf("missing truncation note: %q", lines[len(lines)-1])
	}
}
