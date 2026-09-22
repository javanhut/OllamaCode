package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// grepFixture lays out a workspace with matches in tracked code, a
// dash-named file, an ignored build dir, and a .gitignore'd file.
func grepFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	SetWorkspaceRoot(dir)
	t.Cleanup(func() { SetWorkspaceRoot("") })
	files := map[string]string{
		"main.go":                   "package main\n// needle one\nfunc main() {}\n",
		"my-util.go":                "package main\n// needle two\n",
		"node_modules/pkg/index.js": "// needle vendored\n",
		"generated.txt":             "needle generated\n",
		".gitignore":                "generated.txt\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runGrepTool(t *testing.T, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := GrepTool().Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("grep %v: %v", args, err)
	}
	return out
}

func checkGrepBackend(t *testing.T) {
	out := runGrepTool(t, map[string]any{"pattern": "needle"})
	for _, want := range []string{"main.go", "my-util.go", "needle one", "needle two"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"vendored", "generated"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("ignored content %q leaked into:\n%s", unwanted, out)
		}
	}

	files := runGrepTool(t, map[string]any{"pattern": "needle", "output_mode": "files"})
	if strings.Contains(files, "needle one") || !strings.Contains(files, "my-util.go") {
		t.Errorf("files mode should list paths only:\n%s", files)
	}

	counts := runGrepTool(t, map[string]any{"pattern": "needle", "output_mode": "count", "file_types": ".go"})
	if !strings.Contains(counts, "main.go:1") || strings.Contains(counts, ":0") {
		t.Errorf("count mode wrong:\n%s", counts)
	}

	ctxOut := runGrepTool(t, map[string]any{"pattern": "needle one", "path": "main.go", "context": 1})
	if !strings.Contains(ctxOut, "package main") || !strings.Contains(ctxOut, "func main") {
		t.Errorf("context lines missing:\n%s", ctxOut)
	}

	// Naming an ignored directory explicitly still searches it.
	explicit := runGrepTool(t, map[string]any{"pattern": "needle", "path": "node_modules"})
	if !strings.Contains(explicit, "vendored") {
		t.Errorf("explicit path into an ignored dir found nothing:\n%s", explicit)
	}

	if none := runGrepTool(t, map[string]any{"pattern": "absent-token"}); none != "no matches" {
		t.Errorf("no-match result = %q", none)
	}
}

func TestGrepFallbackSkipsIgnored(t *testing.T) {
	grepFixture(t)
	ripgrepBinary()
	saved := rgPath
	rgPath = ""
	t.Cleanup(func() { rgPath = saved })
	checkGrepBackend(t)
}

func TestGrepRipgrepSkipsIgnored(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg is not installed")
	}
	grepFixture(t)
	ripgrepBinary()
	saved := rgPath
	rgPath = rg
	t.Cleanup(func() { rgPath = saved })
	checkGrepBackend(t)
}
