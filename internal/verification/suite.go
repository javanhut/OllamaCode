package verification

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Suite is the whole-project gate verify mode must pass: formatting, then the
// full test suite. Unlike Detect it is never scoped to the changed files.
type Suite struct {
	Language string
	Manager  string
	Steps    []string
}

func DetectSuite(root string) (Suite, error) {
	switch {
	case exists(filepath.Join(root, "go.mod")):
		return Suite{"go", "go", []string{"! gofmt -l . | grep .", "go test ./..."}}, nil
	case exists(filepath.Join(root, "Cargo.toml")):
		return Suite{"rust", "cargo", []string{"cargo fmt --check", "cargo test"}}, nil
	case exists(filepath.Join(root, "package.json")):
		return nodeSuite(root)
	case isPythonProject(root):
		return pythonSuite(root), nil
	}
	return Suite{}, errors.New("no go.mod, Cargo.toml, package.json or Python manifest found")
}

func nodeSuite(root string) (Suite, error) {
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return Suite{}, err
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return Suite{}, err
	}
	pm, exec := "npm", "npx --no-install"
	switch {
	case exists(filepath.Join(root, "pnpm-lock.yaml")):
		pm, exec = "pnpm", "pnpm exec"
	case exists(filepath.Join(root, "yarn.lock")):
		pm, exec = "yarn", "yarn"
	case exists(filepath.Join(root, "bun.lock")), exists(filepath.Join(root, "bun.lockb")):
		pm, exec = "bun", "bunx"
	}
	var format string
	for _, script := range []string{"format:check", "fmt:check", "check-format"} {
		if pkg.Scripts[script] != "" {
			format = pm + " run " + script
			break
		}
	}
	if format == "" {
		switch {
		case pkg.DevDependencies["@biomejs/biome"] != "" || pkg.Dependencies["@biomejs/biome"] != "":
			format = exec + " biome format ."
		case pkg.DevDependencies["prettier"] != "" || pkg.Dependencies["prettier"] != "":
			format = exec + " prettier --check ."
		default:
			return Suite{}, errors.New("package.json has no format:check script and neither prettier nor biome is a dependency")
		}
	}
	if pkg.Scripts["test"] == "" {
		return Suite{}, errors.New("package.json has no test script")
	}
	return Suite{"javascript", pm, []string{format, pm + " test"}}, nil
}

func pythonSuite(root string) Suite {
	pm, run := "pip", ""
	switch {
	case exists(filepath.Join(root, "uv.lock")):
		pm, run = "uv", "uv run "
	case exists(filepath.Join(root, "poetry.lock")):
		pm, run = "poetry", "poetry run "
	case exists(filepath.Join(root, "Pipfile.lock")):
		pm, run = "pipenv", "pipenv run "
	}
	format := run + "ruff format --check ."
	if data, _ := os.ReadFile(filepath.Join(root, "pyproject.toml")); strings.Contains(string(data), "[tool.black]") {
		format = run + "black --check ."
	}
	python := run + "python"
	if run == "" {
		python = cmp.Or(pythonBin(), "python3")
	}
	return Suite{"python", pm, []string{format, python + " -m pytest -q"}}
}

var sourceExts = map[string]bool{
	".go": true, ".rs": true, ".py": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
}

// Untested returns the changed source files that have no unit test: Go needs a
// _test.go in the same package, Rust a #[cfg(test)] module or tests/*.rs, and
// Python/JS a test file named after the source file anywhere in the tree.
func Untested(root string, changed []string) []string {
	var stems map[string]bool
	var out []string
	for _, path := range changed {
		rel, ok := relTo(root, path)
		abs := filepath.Join(root, rel)
		base := filepath.Base(rel)
		if !ok || !sourceExts[filepath.Ext(rel)] || isTestFile(rel) || notUnitTestable[base] ||
			strings.HasSuffix(base, ".d.ts") || !exists(abs) {
			continue
		}
		var tested bool
		switch filepath.Ext(rel) {
		case ".go":
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(abs), "*_test.go"))
			tested = len(matches) > 0
		case ".rs":
			data, _ := os.ReadFile(abs)
			integration, _ := filepath.Glob(filepath.Join(root, "tests", "*.rs"))
			tested = strings.Contains(string(data), "#[cfg(test)]") || len(integration) > 0
		default:
			if stems == nil {
				stems = testStems(root)
			}
			tested = stems[stem(rel)]
		}
		if !tested {
			out = append(out, rel)
		}
	}
	return out
}

// notUnitTestable are sources that are never tested directly.
var notUnitTestable = map[string]bool{"__init__.py": true, "conftest.py": true, "setup.py": true}

func isTestFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasSuffix(base, "_test.go") || isPyTestFile(rel) ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

// stem strips extensions and test affixes: foo.test.ts, test_foo.py → foo.
func stem(rel string) string {
	base := filepath.Base(rel)
	base, _, _ = strings.Cut(base, ".")
	base = strings.TrimPrefix(base, "test_")
	return strings.TrimSuffix(base, "_test")
}

func testStems(root string) map[string]bool {
	stems := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if sourceExts[filepath.Ext(name)] && isTestFile(name) {
			stems[stem(name)] = true
		}
		return nil
	})
	return stems
}
