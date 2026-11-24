package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codingTasks are realistic multi-step fixtures: a small Go module, a task
// phrased the way a person would phrase it, and an objective check (the
// module's own tests). Unlike the micro-fixtures in evalTasks they need the
// model to read, locate, edit and verify — the loop a coding harness exists for.
func codingTasks() []task {
	const gomod = "module example.com/app\n\ngo 1.22\n"

	statsTest := `package stats

import "testing"

func TestMean(t *testing.T) {
	if got := Mean([]float64{2, 4, 6}); got != 4 {
		t.Fatalf("Mean = %v, want 4", got)
	}
}

func TestMedian(t *testing.T) {
	if got := Median([]float64{9, 1, 5}); got != 5 {
		t.Fatalf("Median odd = %v, want 5", got)
	}
	if got := Median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("Median even = %v, want 2.5", got)
	}
}
`
	statsGood := `package stats

import "sort"

// Mean returns the arithmetic mean of xs.
func Mean(xs []float64) float64 {
	return sum(xs) / float64(len(xs))
}

// Median returns the middle value of xs.
func Median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
`
	slugTest := `package slug

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello, World!":         "hello-world",
		"  Go   is fun  ":       "go-is-fun",
		"Already-a-slug":        "already-a-slug",
		"Tabs\tand_underscores": "tabs-and-underscores",
		"---":                   "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
`
	slugGood := `package slug

import "strings"

// Slugify lowercases s, keeps ASCII letters and digits, and joins the runs of
// kept characters with single hyphens.
func Slugify(s string) string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(words, "-")
}
`
	greetOld := "package greet\n\n// Greet returns a greeting for name.\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name + \"!\"\n}\n"
	greetNew := "package greet\n\n// Greet returns greeting addressed to name.\nfunc Greet(name, greeting string) string {\n\treturn greeting + \", \" + name + \"!\"\n}\n"
	mainOld := "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/app/greet\"\n)\n\nfunc main() {\n\tfmt.Println(greet.Greet(\"world\"))\n}\n"
	mainNew := strings.Replace(mainOld, `greet.Greet("world")`, `greet.Greet("world", "Hello")`, 1)
	reportOld := "package report\n\nimport \"example.com/app/greet\"\n\n// Header greets every name in order.\nfunc Header(names []string) []string {\n\tout := make([]string, 0, len(names))\n\tfor _, n := range names {\n\t\tout = append(out, greet.Greet(n))\n\t}\n\treturn out\n}\n"
	reportNew := strings.Replace(reportOld, "greet.Greet(n)", `greet.Greet(n, "Hello")`, 1)
	reportTest := "package report\n\nimport \"testing\"\n\nfunc TestHeader(t *testing.T) {\n\tgot := Header([]string{\"a\", \"b\"})\n\tif len(got) != 2 || got[0] != \"Hello, a!\" || got[1] != \"Hello, b!\" {\n\t\tt.Fatalf(\"Header = %q\", got)\n\t}\n}\n"

	configGo := "package config\n\nimport \"strings\"\n\n// Parse reads KEY=VALUE lines. Blank lines and lines starting with # are ignored.\nfunc Parse(text string) map[string]string {\n\tout := map[string]string{}\n\tfor _, line := range strings.Split(text, \"\\n\") {\n\t\tline = strings.TrimSpace(line)\n\t\tif line == \"\" || strings.HasPrefix(line, \"#\") {\n\t\t\tcontinue\n\t\t}\n\t\tk, v := splitPair(line)\n\t\tout[k] = v\n\t}\n\treturn out\n}\n"
	pairBad := "package config\n\nimport \"strings\"\n\n// splitPair splits \"k=v\" at the first '=' and trims both sides.\nfunc splitPair(line string) (string, string) {\n\ti := strings.LastIndex(line, \"=\")\n\tif i < 0 {\n\t\treturn strings.TrimSpace(line), \"\"\n\t}\n\treturn strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])\n}\n"
	configTest := "package config\n\nimport \"testing\"\n\nfunc TestParseKeepsEqualsInValue(t *testing.T) {\n\tgot := Parse(\"# db\\nDSN = user=app password=x\\n\\nMODE=prod\\n\")\n\tif got[\"DSN\"] != \"user=app password=x\" {\n\t\tt.Fatalf(\"DSN = %q\", got[\"DSN\"])\n\t}\n\tif got[\"MODE\"] != \"prod\" {\n\t\tt.Fatalf(\"MODE = %q\", got[\"MODE\"])\n\t}\n}\n"

	return []task{
		// Two independent bugs in one file; the model has to run (or read) the
		// tests to find both, and must not "fix" the tests instead.
		{
			Name:   "fix-failing-tests",
			Prompt: "`go test ./...` fails in this module. Fix the implementation so all tests pass. Do not modify any _test.go file.",
			Setup: map[string]string{
				"go.mod":              gomod,
				"stats/stats.go":      strings.Replace(strings.Replace(statsGood, "s := append([]float64(nil), xs...)\n\tsort.Float64s(s)\n", "s := xs\n", 1), "import \"sort\"\n\n", "", 1),
				"stats/stats_test.go": statsTest,
				"stats/sum.go":        "package stats\n\nfunc sum(xs []float64) float64 {\n\tt := 0.0\n\tfor i := 1; i < len(xs); i++ {\n\t\tt += xs[i]\n\t}\n\treturn t\n}\n",
			},
			Check: goTestsPassUnchanged(map[string]string{"stats/stats_test.go": statsTest}),
			Script: []scriptStep{
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go test ./..."}`}}},
				{Calls: []scriptCall{{Name: "read_file", Args: `{"path":"stats/stats.go"}`}, {Name: "read_file", Args: `{"path":"stats/sum.go"}`}}},
				{Calls: []scriptCall{
					{Name: "write_file", Args: mustJSON(map[string]string{"path": "stats/stats.go", "content": statsGood})},
					{Name: "edit_file", Args: `{"path":"stats/sum.go","old_string":"i := 1","new_string":"i := 0"}`},
				}},
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go test ./..."}`}}},
				{Content: "Fixed sum (skipped the first element) and Median (did not sort, ignored even lengths). Tests pass."},
			},
		},
		// Implement from a spec that lives only in the tests, including edge
		// cases (non-ASCII, leading/trailing separators, all-separator input).
		{
			Name:   "implement-to-spec",
			Prompt: "Implement Slugify in slug/slug.go so that the tests in slug/slug_test.go pass. Do not modify the test file.",
			Setup: map[string]string{
				"go.mod":            gomod,
				"slug/slug.go":      "package slug\n\n// Slugify converts s into a URL slug.\nfunc Slugify(s string) string {\n\tpanic(\"TODO\")\n}\n",
				"slug/slug_test.go": slugTest,
			},
			Check: goTestsPassUnchanged(map[string]string{"slug/slug_test.go": slugTest}),
			Script: []scriptStep{
				{Calls: []scriptCall{{Name: "read_file", Args: `{"path":"slug/slug_test.go"}`}, {Name: "read_file", Args: `{"path":"slug/slug.go"}`}}},
				{Calls: []scriptCall{{Name: "write_file", Args: mustJSON(map[string]string{"path": "slug/slug.go", "content": slugGood})}}},
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go test ./..."}`}}},
				{Content: "Implemented Slugify; tests pass."},
			},
		},
		// A signature change that ripples through callers in other packages;
		// only a build tells the model whether it found them all.
		{
			Name:   "change-signature",
			Prompt: "Change greet.Greet to take a second parameter `greeting string` and return greeting + \", \" + name + \"!\". Update every caller to pass \"Hello\" so behavior is unchanged. `go build ./...` and `go test ./...` must pass.",
			Setup: map[string]string{
				"go.mod":                gomod,
				"greet/greet.go":        greetOld,
				"main.go":               mainOld,
				"report/report.go":      reportOld,
				"report/report_test.go": reportTest,
			},
			Check: func(dir, output string) (bool, string) {
				if ok, detail := goTestsPassUnchanged(map[string]string{"report/report_test.go": reportTest})(dir, output); !ok {
					return false, detail
				}
				// Behavioral, not textual: any spelling of the signature is fine
				// as long as greeting is the second argument.
				probe := filepath.Join(dir, "greet", "zz_probe_test.go")
				src := "package greet\n\nimport \"testing\"\n\nfunc TestProbe(t *testing.T) {\n\tif got := Greet(\"x\", \"Hi\"); got != \"Hi, x!\" {\n\t\tt.Fatalf(\"Greet(name, greeting) = %q\", got)\n\t}\n}\n"
				if err := os.WriteFile(probe, []byte(src), 0o644); err != nil {
					return false, err.Error()
				}
				defer os.Remove(probe)
				if ok, detail := goTestsPassUnchanged(nil)(dir, output); !ok {
					return false, "Greet(\"x\", \"Hi\") != \"Hi, x!\": " + detail
				}
				return true, "signature changed and callers updated"
			},
			Script: []scriptStep{
				{Calls: []scriptCall{{Name: "grep", Args: `{"pattern":"Greet\\("}`}}},
				{Calls: []scriptCall{{Name: "read_file", Args: `{"path":"greet/greet.go"}`}, {Name: "read_file", Args: `{"path":"main.go"}`}, {Name: "read_file", Args: `{"path":"report/report.go"}`}}},
				{Calls: []scriptCall{
					{Name: "write_file", Args: mustJSON(map[string]string{"path": "greet/greet.go", "content": greetNew})},
					{Name: "write_file", Args: mustJSON(map[string]string{"path": "main.go", "content": mainNew})},
					{Name: "write_file", Args: mustJSON(map[string]string{"path": "report/report.go", "content": reportNew})},
				}},
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go build ./... && go test ./..."}`}}},
				{Content: "Changed the signature and updated both callers; build and tests pass."},
			},
		},
		// The failing test names Parse, but the bug is in a helper in another
		// file. Rewards following the evidence over editing where the symptom is.
		{
			Name:   "bug-in-helper",
			Prompt: "A user reports that values containing '=' are cut off when parsing config (see the failing test). Find the root cause and fix it. Do not modify tests.",
			Setup: map[string]string{
				"go.mod":                gomod,
				"config/config.go":      configGo,
				"config/pair.go":        pairBad,
				"config/config_test.go": configTest,
			},
			Check: func(dir, output string) (bool, string) {
				b, _ := os.ReadFile(filepath.Join(dir, "config", "config.go"))
				if string(b) != configGo {
					return false, "edited config.go; the root cause is splitPair in pair.go"
				}
				return goTestsPassUnchanged(map[string]string{"config/config_test.go": configTest})(dir, output)
			},
			Script: []scriptStep{
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go test ./..."}`}}},
				{Calls: []scriptCall{{Name: "read_file", Args: `{"path":"config/config.go"}`}, {Name: "read_file", Args: `{"path":"config/pair.go"}`}}},
				{Calls: []scriptCall{{Name: "edit_file", Args: `{"path":"config/pair.go","old_string":"strings.LastIndex","new_string":"strings.Index"}`}}},
				{Calls: []scriptCall{{Name: "run_shell", Args: `{"command":"go test ./..."}`}}},
				{Content: "splitPair split at the last '=' instead of the first; fixed in config/pair.go."},
			},
		},
	}
}

// goTestsPassUnchanged checks that `go test ./...` passes in dir and that the
// listed files (the tests) still hold their original contents.
func goTestsPassUnchanged(frozen map[string]string) func(dir, output string) (bool, string) {
	return func(dir, _ string) (bool, string) {
		for rel, want := range frozen {
			b, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil || string(b) != want {
				return false, rel + " was modified or removed"
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "test", "./...")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			text := strings.TrimSpace(string(out))
			if len(text) > 300 {
				text = text[len(text)-300:]
			}
			return false, "go test failed: " + text
		}
		return true, "go test passes"
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
