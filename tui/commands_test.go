package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCustomCommandFrontmatter(t *testing.T) {
	c := parseCustomCommand("---\ndescription: \"Review a diff\"\nmode: explore\nagent: build\n---\nReview $ARGUMENTS carefully.\n")
	if c.description != "Review a diff" || c.mode != "explore" || c.template != "Review $ARGUMENTS carefully." {
		t.Fatalf("unexpected parse: %+v", c)
	}
	c = parseCustomCommand("Summarize the repo layout.\nBe brief.")
	if c.description != "Summarize the repo layout." || c.mode != "" {
		t.Fatalf("body-derived description: %+v", c)
	}
}

func TestExpandTemplate(t *testing.T) {
	cases := []struct{ tmpl, args, want string }{
		{"Fix $ARGUMENTS now", "the bug in foo", "Fix the bug in foo now"},
		{"Rename $1 to $2", `Foo "Bar Baz" extra`, "Rename Foo to Bar Baz extra"},
		{"Only $1", "", "Only "},
		{"No placeholders", "tail args", "No placeholders\n\ntail args"},
		{"No placeholders", "", "No placeholders"},
	}
	for _, c := range cases {
		if got := expandTemplate(c.tmpl, c.args); got != c.want {
			t.Errorf("expandTemplate(%q, %q) = %q, want %q", c.tmpl, c.args, got, c.want)
		}
	}
}

func TestExpandShellGatesUntrustedCommands(t *testing.T) {
	if got := expandShell("x !`echo hi` y", true); got != "x hi y" {
		t.Fatalf("trusted expansion: %q", got)
	}
	got := expandShell("x !`touch /tmp/should-not-exist-ocode` y", false)
	if !strings.Contains(got, "not run") {
		t.Fatalf("untrusted mutating command must not run: %q", got)
	}
	if got := expandShell("!`echo ok`", false); got != "ok" {
		t.Fatalf("read-only untrusted command should run: %q", got)
	}
}

func TestLoadCustomCommandsNamespacesAndSkipsBuiltins(t *testing.T) {
	low, high := t.TempDir(), t.TempDir()
	must := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(low, "review.md"), "global review")
	must(filepath.Join(high, "review.md"), "project review")
	must(filepath.Join(high, "git", "pr.md"), "open a PR")
	must(filepath.Join(high, "undo.md"), "must not shadow the built-in")
	must(filepath.Join(high, "notes.txt"), "not markdown")
	cmds := loadCustomCommands([]string{low, high})
	byName := map[string]string{}
	for _, c := range cmds {
		byName[c.name] = c.template
	}
	if len(byName) != 2 || byName["/review"] != "project review" || byName["/git:pr"] != "open a PR" {
		t.Fatalf("unexpected commands: %v", byName)
	}
}
