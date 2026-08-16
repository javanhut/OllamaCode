package verification

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Plan struct {
	Command string
	Label   string
	Steps   []string
}

// Detect builds a conservative verification plan. It only derives commands
// from known project manifests and changed paths; arbitrary project scripts are
// never auto-discovered. An explicit override remains a user trust decision.
func Detect(root string, changed []string, override string) (Plan, bool) {
	if command := strings.TrimSpace(override); command != "" {
		return Plan{Command: command, Label: "verify", Steps: []string{command}}, true
	}
	if exists(filepath.Join(root, "go.mod")) {
		steps := goSteps(root, changed)
		steps = append(steps, "go build ./...")
		return Plan{Command: strings.Join(steps, " && "), Label: "targeted Go tests + build", Steps: steps}, true
	}
	if exists(filepath.Join(root, "Cargo.toml")) {
		steps := []string{"cargo test --no-run --quiet", "cargo check --quiet"}
		return Plan{Command: strings.Join(steps, " && "), Label: "cargo test --no-run + check", Steps: steps}, true
	}
	if isPythonProject(root) {
		if steps := pySteps(root, changed); len(steps) > 0 {
			return Plan{Command: strings.Join(steps, " && "), Label: "python compile + targeted tests", Steps: steps}, true
		}
	}
	if exists(filepath.Join(root, "tsconfig.json")) {
		steps := []string{"npx --no-install tsc --noEmit"}
		return Plan{Command: steps[0], Label: "tsc --noEmit", Steps: steps}, true
	}
	return Plan{}, false
}

// isPythonProject reports whether root carries a Python manifest. Checked after
// the Go and Cargo arms so a polyglot repo still gets its primary build.
func isPythonProject(root string) bool {
	for _, name := range []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"} {
		if exists(filepath.Join(root, name)) {
			return true
		}
	}
	return false
}

// pySteps builds the Python arm: a byte-compile of the changed files, which is
// the cheapest objective "does this even parse" floor and the closest thing
// Python has to `go build`, plus pytest scoped to the changed test files.
//
// Type checkers are deliberately absent here. pyright and mypy report findings
// in code the turn never touched, and a pass/fail gate on those would trap the
// model repairing someone else's annotations; they run as informational lint
// instead (see lint.go). Returns nil when no .py file changed, which lets
// Detect fall through to the remaining arms rather than claim a project it
// cannot check.
func pySteps(root string, changed []string) []string {
	python := pythonBin()
	if python == "" {
		return nil
	}
	var files, testFiles []string
	for _, path := range changed {
		if filepath.Ext(path) != ".py" {
			continue
		}
		rel, ok := relTo(root, path)
		if !ok {
			continue
		}
		files = append(files, rel)
		if isPyTestFile(rel) {
			testFiles = append(testFiles, rel)
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	sort.Strings(testFiles)
	steps := []string{python + " -m compileall -q " + strings.Join(files, " ")}
	if len(testFiles) > 0 {
		if _, err := exec.LookPath("pytest"); err == nil {
			steps = append(steps, "pytest -q "+strings.Join(testFiles, " "))
		}
	}
	return steps
}

// pythonBin prefers python3 and falls back to python; "" when neither is
// installed, which drops the Python arm entirely rather than emitting a command
// that cannot run.
func pythonBin() string {
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// isPyTestFile matches both pytest naming conventions.
func isPyTestFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
}

// relTo expresses path relative to root, reporting ok=false for anything that
// escapes it.
func relTo(root, path string) (string, bool) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func goSteps(root string, changed []string) []string {
	ordered := goChangedPackages(root, changed)
	steps := make([]string, 0, len(ordered))
	for _, pkg := range ordered {
		steps = append(steps, "go test "+pkg)
	}
	return steps
}

// goChangedPackages maps changed .go files to their package patterns,
// sorted for deterministic command lines.
func goChangedPackages(root string, changed []string) []string {
	packages := map[string]bool{}
	for _, path := range changed {
		if filepath.Ext(path) != ".go" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		rel, err := filepath.Rel(root, filepath.Dir(filepath.Clean(path)))
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "."
		if rel != "." {
			pkg = "./" + filepath.ToSlash(rel)
		}
		packages[pkg] = true
	}
	ordered := make([]string, 0, len(packages))
	for pkg := range packages {
		ordered = append(ordered, pkg)
	}
	sort.Strings(ordered)
	return ordered
}

// Fingerprint identifies the exact changed-file state covered by verification.
func Fingerprint(root string, changed []string) string {
	paths := append([]string(nil), changed...)
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		clean := path
		if !filepath.IsAbs(clean) {
			clean = filepath.Join(root, clean)
		}
		fmt.Fprintf(h, "%s\x00", filepath.Clean(path))
		if data, err := os.ReadFile(clean); err == nil {
			h.Write(data)
		} else {
			fmt.Fprintf(h, "missing:%v", err)
		}
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
