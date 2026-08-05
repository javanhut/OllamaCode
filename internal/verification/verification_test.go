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
