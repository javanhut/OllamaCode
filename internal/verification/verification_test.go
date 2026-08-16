package verification

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectGoTargetsChangedPackagesBeforeBuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, ok := Detect(dir, []string{"internal/a/a.go", "main.go", "README.md"}, "")
	if !ok {
		t.Fatal("expected Go plan")
	}
	want := []string{"go test .", "go test ./internal/a", "go build ./..."}
	if !reflect.DeepEqual(plan.Steps, want) {
		t.Fatalf("got %#v want %#v", plan.Steps, want)
	}
}

// fakeBins puts stub executables on an otherwise empty PATH, so the Python arm
// is exercised without depending on what the host happens to have installed.
func fakeBins(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestDetectPythonCompilesChangedFiles(t *testing.T) {
	fakeBins(t, "python3") // no pytest on PATH
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, ok := Detect(dir, []string{"pkg/a.py", "README.md"}, "")
	if !ok {
		t.Fatal("expected Python plan")
	}
	want := []string{"python3 -m compileall -q pkg/a.py"}
	if !reflect.DeepEqual(plan.Steps, want) {
		t.Fatalf("got %#v want %#v", plan.Steps, want)
	}
}

func TestDetectPythonAddsTargetedPytest(t *testing.T) {
	fakeBins(t, "python3", "pytest")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, ok := Detect(dir, []string{"a.py", "tests/test_a.py"}, "")
	if !ok {
		t.Fatal("expected Python plan")
	}
	want := []string{
		"python3 -m compileall -q a.py tests/test_a.py",
		"pytest -q tests/test_a.py",
	}
	if !reflect.DeepEqual(plan.Steps, want) {
		t.Fatalf("got %#v want %#v", plan.Steps, want)
	}
}

// A Python manifest with no changed .py files must not claim the turn — the
// remaining arms still get their chance.
func TestDetectPythonFallsThroughWithoutPythonChanges(t *testing.T) {
	fakeBins(t, "python3")
	dir := t.TempDir()
	for _, name := range []string{"requirements.txt", "tsconfig.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan, ok := Detect(dir, []string{"app.ts"}, "")
	if !ok || plan.Label != "tsc --noEmit" {
		t.Fatalf("expected fall-through to tsc, got ok=%v label=%q", ok, plan.Label)
	}
}

func TestDetectPythonSkippedWithoutInterpreter(t *testing.T) {
	fakeBins(t) // empty PATH
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := Detect(dir, []string{"a.py"}, ""); ok {
		t.Fatal("expected no plan when no interpreter is installed")
	}
}

func TestFingerprintChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	_ = os.WriteFile(path, []byte("one"), 0o644)
	a := Fingerprint(dir, []string{"x.go"})
	_ = os.WriteFile(path, []byte("two"), 0o644)
	b := Fingerprint(dir, []string{"x.go"})
	if a == b {
		t.Fatal("fingerprint did not change")
	}
}
