package tui

import (
	"strings"
	"testing"
)

func TestRepairDetailAppendsLintDiagnostics(t *testing.T) {
	got := repairDetail("build failed: undefined: x", "main.go:12:5: err is never used (SA4006)")
	if !strings.Contains(got, "build failed") || !strings.Contains(got, "main.go:12:5") {
		t.Fatalf("repair message must carry both the check output and lint diagnostics:\n%s", got)
	}
	if !strings.Contains(got, "Linter diagnostics") {
		t.Fatalf("lint section needs a header so the model can tell it from compiler output:\n%s", got)
	}
}

func TestRepairDetailWithoutLintIsUnchanged(t *testing.T) {
	if got := repairDetail("build failed", ""); got != "build failed" {
		t.Fatalf("no linter must mean byte-identical output, got %q", got)
	}
}
