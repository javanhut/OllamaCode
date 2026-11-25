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

func TestVerifyErrorSignatureIgnoresShiftingNumbers(t *testing.T) {
	a := verifyErrorSignature("main.go:12:5: undefined: foo\nFAIL 0.31s")
	b := verifyErrorSignature("main.go:14:9: undefined: foo\nFAIL 0.42s")
	if a != b {
		t.Fatalf("same error at a shifted line must match:\n%s\n%s", a, b)
	}
	if a == verifyErrorSignature("main.go:12:5: undefined: bar") {
		t.Fatal("a different error must not match")
	}
}

// TestVerifyStuckOnRepeatedError: a second identical failure asks for a new
// approach; a third stops editing instead of burning the remaining attempts.
func TestVerifyStuckOnRepeatedError(t *testing.T) {
	m := routedModel(nil, "small")
	fail := func(out string) string {
		return m.verifyFailureAdvice(verifyDoneMsg{ok: false, label: "go build", output: out})
	}
	if got := fail("a.go:1:1: undefined: x"); strings.Contains(got, "SAME ERROR") || strings.Contains(got, "STUCK") {
		t.Fatalf("first failure is ordinary: %s", got)
	}
	if got := fail("a.go:2:1: undefined: x"); !strings.Contains(got, "SAME ERROR") {
		t.Fatalf("second identical failure must ask for a different approach: %s", got)
	}
	if got := fail("a.go:3:1: undefined: x"); !strings.Contains(got, "VERIFICATION STUCK") {
		t.Fatalf("third identical failure must stop: %s", got)
	}
	m2 := routedModel(nil, "small")
	m2.verifyFailureAdvice(verifyDoneMsg{ok: false, label: "go build", output: "undefined: x"})
	if got := m2.verifyFailureAdvice(verifyDoneMsg{ok: false, label: "go build", output: "undefined: y"}); strings.Contains(got, "SAME ERROR") {
		t.Fatalf("a changed error is progress, not stuck: %s", got)
	}
}
