package verification

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Test-integrity checks. A model told "make the tests pass" has a cheaper
// route than fixing the code: delete the failing test, skip it, or rewrite the
// harness that decides pass/fail. Frontier models do this in the wild (METR,
// June 2025: o3 reward-hacked every run of one RE-Bench task; a model-added
// conftest.py can rewrite pytest outcomes), and the verification gate is
// exactly such an objective, so the gate's own success is suspect when the
// same turn touched the tests. These checks WARN — a test can be legitimately
// removed or skipped — and the warning goes to both the user and the model.

// TestFileChange is one changed path with its content before and after the
// turn. Before == "" with BeforeExists false means the file is new; After ==
// "" with AfterExists false means it was deleted.
type TestFileChange struct {
	Path         string
	Before       string
	BeforeExists bool
	After        string
	AfterExists  bool
}

// IsTestPath reports whether a path is a test file or test/CI infrastructure.
func IsTestPath(path string) bool {
	return testFileKind(path) != "" || isTestInfra(path)
}

var jsTestFile = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)

func testFileKind(path string) string {
	base := strings.ToLower(filepath.Base(path))
	slash := filepath.ToSlash(strings.ToLower(path))
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return "go"
	case strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")):
		return "python"
	case jsTestFile.MatchString(base):
		return "js"
	case strings.HasSuffix(base, ".rs") && (strings.Contains(slash, "/tests/") || strings.HasPrefix(slash, "tests/")):
		return "rust"
	case strings.HasSuffix(base, "test.java") || strings.HasSuffix(base, "tests.java"):
		return "java"
	}
	return ""
}

func isTestInfra(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	slash := filepath.ToSlash(strings.ToLower(path))
	switch base {
	case "conftest.py", "pytest.ini", "tox.ini", "jest.config.js", "jest.config.ts", "vitest.config.ts", "vitest.config.js", ".mocharc.json", ".mocharc.js":
		return true
	}
	return strings.HasPrefix(slash, ".github/workflows/") || strings.Contains(slash, "/.github/workflows/") ||
		base == ".gitlab-ci.yml"
}

// testCaseMarkers count test cases per language; skipMarkers count disabled
// ones. Counting is deliberately crude: a drop in cases or a rise in skips is
// a signal for a human to look, not a verdict.
var (
	testCaseMarkers = map[string]*regexp.Regexp{
		"go":     regexp.MustCompile(`(?m)^func (Test|Fuzz)\w*\(`),
		"python": regexp.MustCompile(`(?m)^\s*(async\s+)?def test\w*\(`),
		"js":     regexp.MustCompile(`(?m)\b(it|test)(\.each\([^)]*\))?\s*\(`),
		"rust":   regexp.MustCompile(`(?m)#\[(tokio::)?test\]`),
		"java":   regexp.MustCompile(`(?m)@Test\b`),
	}
	skipMarkers = map[string]*regexp.Regexp{
		"go":     regexp.MustCompile(`\bt\.(Skip|SkipNow|Skipf)\(`),
		"python": regexp.MustCompile(`@pytest\.mark\.(skip|skipif|xfail)\b|\bpytest\.(skip|xfail)\(|@unittest\.(skip|expectedFailure)`),
		"js":     regexp.MustCompile(`\b(it|test|describe)\.(skip|todo)\(|\bx(it|describe|test)\(`),
		"rust":   regexp.MustCompile(`#\[ignore\]`),
		"java":   regexp.MustCompile(`@(Disabled|Ignore)\b`),
	}
	// assertionMarkers: a test that keeps its name but loses its assertions
	// passes vacuously.
	assertionMarkers = map[string]*regexp.Regexp{
		"go":     regexp.MustCompile(`\bt\.(Error|Errorf|Fatal|Fatalf|Fail|FailNow)\(|\b(assert|require)\.\w+\(`),
		"python": regexp.MustCompile(`\bassert\b|\.assert\w*\(|pytest\.raises\(`),
		"js":     regexp.MustCompile(`\bexpect\(|\bassert[.(]`),
		"rust":   regexp.MustCompile(`\bassert(_eq|_ne)?!\(`),
		"java":   regexp.MustCompile(`\bassert\w*\(`),
	}
)

// TestIntegrityWarnings returns one human-readable line per suspicious change.
func TestIntegrityWarnings(changes []TestFileChange) []string {
	var out []string
	for _, c := range changes {
		if isTestInfra(c.Path) && c.BeforeExists && (!c.AfterExists || c.Before != c.After) {
			out = append(out, fmt.Sprintf("%s: test/CI configuration was modified — it can change what counts as passing", c.Path))
			continue
		}
		if isTestInfra(c.Path) && !c.BeforeExists && c.AfterExists && strings.EqualFold(filepath.Base(c.Path), "conftest.py") {
			out = append(out, fmt.Sprintf("%s: a new conftest.py was added — it can rewrite pytest outcomes", c.Path))
			continue
		}
		kind := testFileKind(c.Path)
		if kind == "" || !c.BeforeExists {
			continue // new test files are fine
		}
		if !c.AfterExists {
			out = append(out, fmt.Sprintf("%s: test file deleted", c.Path))
			continue
		}
		count := func(re *regexp.Regexp, s string) int { return len(re.FindAllStringIndex(s, -1)) }
		if before, after := count(testCaseMarkers[kind], c.Before), count(testCaseMarkers[kind], c.After); after < before {
			out = append(out, fmt.Sprintf("%s: test cases went from %d to %d", c.Path, before, after))
		}
		if before, after := count(skipMarkers[kind], c.Before), count(skipMarkers[kind], c.After); after > before {
			out = append(out, fmt.Sprintf("%s: %d skip/xfail marker(s) added", c.Path, after-before))
		}
		if before, after := count(assertionMarkers[kind], c.Before), count(assertionMarkers[kind], c.After); before > 0 && after*2 <= before && before-after >= 2 {
			out = append(out, fmt.Sprintf("%s: assertions went from %d to %d", c.Path, before, after))
		}
	}
	return out
}
