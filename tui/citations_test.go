package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/javanhut/ollama_code/api"
)

func TestParseCitations(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []citation
	}{
		{
			name: "simple path:line",
			text: "The mode gate lives in tui/mode.go:206 and refuses the switch.",
			want: []citation{{Raw: "tui/mode.go:206", Path: "tui/mode.go", Line: 206}},
		},
		{
			name: "line range",
			text: "See api/api.go:120-135 for the retry loop.",
			want: []citation{{Raw: "api/api.go:120-135", Path: "api/api.go", Line: 120, EndLine: 135}},
		},
		{
			name: "bare filename with extension",
			text: "main.go:12 wires the flags.",
			want: []citation{{Raw: "main.go:12", Path: "main.go", Line: 12}},
		},
		{
			name: "backticked citation",
			text: "Check `tui/update.go:782` before finishing.",
			want: []citation{{Raw: "tui/update.go:782", Path: "tui/update.go", Line: 782}},
		},
		{
			name: "multiple citations in order",
			text: "a.go:1 calls b/b.go:2 which reads c/c/c.go:3-4.",
			want: []citation{
				{Raw: "a.go:1", Path: "a.go", Line: 1},
				{Raw: "b/b.go:2", Path: "b/b.go", Line: 2},
				{Raw: "c/c/c.go:3-4", Path: "c/c/c.go", Line: 3, EndLine: 4},
			},
		},
		{
			name: "duplicate citations deduped",
			text: "a.go:5 and again a.go:5",
			want: []citation{{Raw: "a.go:5", Path: "a.go", Line: 5}},
		},
		{
			name: "time of day is not a citation",
			text: "we met at 10:30 to discuss it",
		},
		{
			name: "prose colon with number",
			text: "note: 5 items were found, ratio 2:1",
		},
		{
			name: "http URL is not a citation",
			text: "docs at https://example.com:8080/spec and http://x.test/a.go:10",
		},
		{
			name: "markdown link target skipped, link text kept",
			text: "[the gate](tui/mode.go:206) and [tui/mode.go:99](http://x.test) differ",
			want: []citation{{Raw: "tui/mode.go:99", Path: "tui/mode.go", Line: 99}},
		},
		{
			name: "windows drive path",
			text: `on Windows it is C:\src\main.go:10 instead`,
			want: []citation{{Raw: `\src\main.go:10`, Path: `C:\src\main.go`, Line: 10}},
		},
		{
			name: "windows forward-slash drive path",
			text: `also C:/src/main.go:11 works`,
			want: []citation{{Raw: `/src/main.go:11`, Path: `C:/src/main.go`, Line: 11}},
		},
		{
			name: "version-ish numbers are not citations",
			text: "requires go 1.22:3 or later",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCitations(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCitations(%q) = %v, want %v", tt.text, got, tt.want)
			}
			for i, c := range got {
				if c != tt.want[i] {
					t.Errorf("citation %d = %+v, want %+v", i, c, tt.want[i])
				}
			}
		})
	}
}

func TestMakesCodeClaims(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"The gate is in tui/mode.go.", true},
		{"It reads `config.toml` at startup.", true},
		{"A mutex serializes access to shared state.", false},
		{"Exponential backoff doubles the wait each retry.", false},
		{"No files mentioned, just ideas: parsers, lexers.", false},
	}
	for _, tt := range tests {
		if got := makesCodeClaims(tt.text); got != tt.want {
			t.Errorf("makesCodeClaims(%q) = %t, want %t", tt.text, got, tt.want)
		}
	}
}

func TestValidateCitations(t *testing.T) {
	root := t.TempDir()
	writeFile := func(rel string, lines int) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		content := strings.Repeat("line\n", lines)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("a.go", 10)
	writeFile("sub/b.go", 3)
	// A final line without a trailing newline still counts.
	if err := os.WriteFile(filepath.Join(root, "nonewline.go"), []byte("one\ntwo"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		cites    []citation
		wantN    int // number of problems
		contains string
	}{
		{name: "all valid", cites: parseCitations("a.go:1 and sub/b.go:3 and a.go:10-10"), wantN: 0},
		{name: "missing file", cites: parseCitations("ghost.go:5"), wantN: 1, contains: "does not exist"},
		{name: "line beyond end", cites: parseCitations("a.go:11"), wantN: 1, contains: "out of range"},
		{name: "range end beyond end", cites: parseCitations("a.go:8-20"), wantN: 1, contains: "out of range"},
		{name: "line zero", cites: parseCitations("a.go:0"), wantN: 1, contains: "out of range"},
		{name: "reversed range", cites: parseCitations("a.go:9-4"), wantN: 1, contains: "reversed"},
		{name: "unterminated final line counts", cites: parseCitations("nonewline.go:2"), wantN: 0},
		{name: "nested path valid", cites: parseCitations("sub/b.go:2"), wantN: 0},
		{name: "mixed valid and invalid", cites: parseCitations("a.go:5 then ghost.go:1"), wantN: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := validateCitations(root, tt.cites)
			if len(problems) != tt.wantN {
				t.Fatalf("validateCitations = %v, want %d problems", problems, tt.wantN)
			}
			if tt.contains != "" && !strings.Contains(problems[0], tt.contains) {
				t.Errorf("problem %q missing %q", problems[0], tt.contains)
			}
		})
	}
}

func TestCitationProblemsMissingCitations(t *testing.T) {
	root := t.TempDir()
	// Names a source file, cites nothing → one "missing citations" problem.
	problems := citationProblems(root, "The gate in tui/mode.go refuses the switch.")
	if len(problems) != 1 || !strings.Contains(problems[0], "path:line") {
		t.Fatalf("expected a missing-citations problem, got %v", problems)
	}
	// Pure explanation: no files named → no problem even with zero citations.
	if problems := citationProblems(root, "A mutex serializes access to shared state."); problems != nil {
		t.Fatalf("pure explanation should pass, got %v", problems)
	}
}

func TestCitationCorrectionIssued(t *testing.T) {
	correction := msg("system", citationCorrectionMessage([]string{"a.go:5 — file does not exist in the workspace"}))
	tests := []struct {
		name    string
		history []api.Message
		want    bool
	}{
		{
			name:    "correction after last user message",
			history: []api.Message{msg("user", "q"), msg("assistant", "a"), correction},
			want:    true,
		},
		{
			name:    "no correction this turn",
			history: []api.Message{msg("user", "q"), msg("assistant", "a")},
			want:    false,
		},
		{
			name:    "correction in an earlier turn does not count",
			history: []api.Message{msg("user", "q1"), correction, msg("assistant", "a1"), msg("user", "q2"), msg("assistant", "a2")},
			want:    false,
		},
		{
			name:    "empty history",
			history: nil,
			want:    false,
		},
		{
			name:    "other system messages do not count",
			history: []api.Message{msg("user", "q"), msg("system", "[VERIFICATION FAILED] build broke")},
			want:    false,
		},
	}
	for _, tt := range tests {
		if got := citationCorrectionIssued(tt.history); got != tt.want {
			t.Errorf("%s: citationCorrectionIssued = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestCitationGateEarlyReturns(t *testing.T) {
	// These paths all return before startStream, so a bare Model suffices.
	m := &Model{mode: PlanMode}
	if cmd := m.maybeCitationGate("tui/mode.go:999 is broken"); cmd != nil {
		t.Error("gate must stay out of non-explore modes")
	}
	m = &Model{mode: ExploreMode}
	if cmd := m.maybeCitationGate("A mutex serializes access to shared state."); cmd != nil {
		t.Error("pure explanations must not be punished")
	}
	if cmd := m.maybeCitationGate("   "); cmd != nil {
		t.Error("empty answers must pass")
	}
	m = &Model{mode: ExploreMode, history: []api.Message{
		msg("user", "q"),
		msg("assistant", "claims about a.go"),
		msg("system", citationCorrectionMessage([]string{"a.go:5 — file does not exist in the workspace"})),
		msg("assistant", "still claims about a.go without citations"),
	}}
	if cmd := m.maybeCitationGate("still claims about a.go without citations"); cmd != nil {
		t.Error("loop guard: a second correction must never fire in one turn")
	}
}

func TestCitationCorrectionMessage(t *testing.T) {
	out := citationCorrectionMessage([]string{"a.go:5 — file does not exist in the workspace", "b.go:1 — line out of range (b.go has 0 lines)"})
	if !strings.HasPrefix(out, citationCorrectionPrefix) {
		t.Errorf("correction must carry the detection prefix, got %q", out)
	}
	for _, want := range []string{"a.go:5", "b.go:1", "path:line"} {
		if !strings.Contains(out, want) {
			t.Errorf("correction missing %q:\n%s", want, out)
		}
	}
	// Long failure lists are capped.
	many := make([]string, 20)
	for i := range many {
		many[i] = "x"
	}
	if out := citationCorrectionMessage(many); !strings.Contains(out, "and 12 more") {
		t.Errorf("expected truncation notice, got %q", out)
	}
}

func TestStyleCitations(t *testing.T) {
	// Expectations are computed via citationStyle.Render so the tests hold
	// whether or not the test renderer has a color profile (no TTY => plain).
	t.Run("citation styled, text unchanged", func(t *testing.T) {
		in := "The gate lives in tui/mode.go:206 today."
		want := "The gate lives in " + citationStyle.Render("tui/mode.go:206") + " today."
		if out := styleCitations(in); out != want {
			t.Errorf("styleCitations = %q, want %q", out, want)
		}
		if stripped := ansi.Strip(styleCitations(in)); stripped != in {
			t.Errorf("printable text changed: %q -> %q", in, stripped)
		}
	})
	t.Run("line without citations untouched", func(t *testing.T) {
		in := "we met at 10:30, note: 5 items"
		if out := styleCitations(in); out != in {
			t.Errorf("expected no change, got %q", out)
		}
	})
	t.Run("surrounding ANSI style is re-applied after the citation", func(t *testing.T) {
		bold := "\x1b[1m"
		reset := "\x1b[0m"
		in := bold + "see a.go:5 for details" + reset
		out := styleCitations(in)
		idx := strings.Index(out, "for details")
		if idx < 0 {
			t.Fatalf("lost trailing text: %q", out)
		}
		// The text after the citation must re-open the bold attribute that
		// the citation's own reset would otherwise kill.
		if !strings.Contains(out[:idx], "\x1b[1m") {
			t.Errorf("bold style not restored before trailing text: %q", out)
		}
		if stripped := ansi.Strip(out); stripped != ansi.Strip(in) {
			t.Errorf("printable text changed: %q -> %q", ansi.Strip(in), stripped)
		}
	})
	t.Run("range wins over its single-line prefix", func(t *testing.T) {
		in := "a.go:10-12 covers it"
		want := citationStyle.Render("a.go:10-12") + " covers it"
		if out := styleCitations(in); out != want {
			t.Errorf("range not styled as a unit: %q, want %q", out, want)
		}
	})
}
