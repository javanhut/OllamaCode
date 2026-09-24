package verification

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDetectSuite(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		manager string
		steps   []string
	}{
		{"go", map[string]string{"go.mod": ""}, "go", []string{"! gofmt -l . | grep .", "go test ./..."}},
		{"rust", map[string]string{"Cargo.toml": ""}, "cargo", []string{"cargo fmt --check", "cargo test"}},
		{"pnpm prettier", map[string]string{
			"package.json":   `{"scripts":{"test":"vitest"},"devDependencies":{"prettier":"3"}}`,
			"pnpm-lock.yaml": "",
		}, "pnpm", []string{"pnpm exec prettier --check .", "pnpm test"}},
		{"npm format script", map[string]string{
			"package.json": `{"scripts":{"test":"jest","format:check":"prettier -c ."}}`,
		}, "npm", []string{"npm run format:check", "npm test"}},
		{"uv black", map[string]string{"pyproject.toml": "[tool.black]\n", "uv.lock": ""}, "uv",
			[]string{"uv run black --check .", "uv run python -m pytest -q"}},
	}
	for _, c := range cases {
		suite, err := DetectSuite(writeFiles(t, c.files))
		if err != nil || suite.Manager != c.manager || !slices.Equal(suite.Steps, c.steps) {
			t.Errorf("%s: got %+v, %v", c.name, suite, err)
		}
	}
	for name, files := range map[string]map[string]string{
		"no manifest":  {"README.md": ""},
		"no formatter": {"package.json": `{"scripts":{"test":"jest"}}`},
		"no test":      {"package.json": `{"devDependencies":{"prettier":"3"}}`},
	} {
		if _, err := DetectSuite(writeFiles(t, files)); err == nil {
			t.Errorf("%s: want an error, nothing can pass without a suite", name)
		}
	}
}

func TestUntested(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"a/a.go": "", "a/a_test.go": "",
		"b/b.go":    "",
		"src/ok.ts": "", "test/ok.test.ts": "",
		"src/bare.ts": "",
		"pkg/mod.py":  "", "tests/test_mod.py": "",
		"pkg/lonely.py": "", "pkg/__init__.py": "",
		"lib.rs":                      "#[cfg(test)]\nmod tests {}",
		"bin.rs":                      "fn main() {}",
		"node_modules/x/bare.test.ts": "",
	})
	changed := []string{"a/a.go", "a/a_test.go", "b/b.go", "src/ok.ts", "src/bare.ts",
		"pkg/mod.py", "pkg/lonely.py", "pkg/__init__.py", "lib.rs", "bin.rs", "gone.go", "README.md"}
	got := Untested(root, changed)
	want := []string{"b/b.go", "src/bare.ts", "pkg/lonely.py", "bin.rs"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
