package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/tools"
)

// mentionTestDir chdirs into a fresh tempdir and unpins the jail root so the
// per-call derivation (cwd = workspace) governs containment — other tests in
// this package pin the real repo root via New().
func mentionTestDir(t *testing.T) string {
	t.Helper()
	tools.SetWorkspaceRoot("")
	t.Cleanup(func() { tools.SetWorkspaceRoot("") })
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMentionPathTokenRules(t *testing.T) {
	cases := []struct {
		word string
		want string
		ok   bool
	}{
		{"@main.go", "main.go", true},
		{"@tui/keys.go", "tui/keys.go", true},
		{"@.gitignore", ".gitignore", true},
		{"(@foo.go):", "foo.go", true},  // leading bracket + trailing colon
		{"@foo.go.", "foo.go", true},    // sentence-final dot
		{"@foo/bar,", "foo/bar", true},  // trailing comma
		{"@user", "", false},            // handle: no '/' or '.'
		{"user@example.com", "", false}, // doesn't start with '@'
		{"@", "", false},                // bare marker
		{"plain", "", false},
	}
	for _, c := range cases {
		got, ok := mentionPath(c.word)
		if got != c.want || ok != c.ok {
			t.Errorf("mentionPath(%q) = (%q, %v), want (%q, %v)", c.word, got, ok, c.want, c.ok)
		}
	}
}

func TestFindMentionsDedupAndCap(t *testing.T) {
	got := findMentions("see @a.go and @b.go, then @a.go again")
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("findMentions dedup/order: %v", got)
	}
	var words []string
	for range maxMentionsPerMessage + 3 {
		words = append(words, "@f"+strings.Repeat("x", len(words))+".go")
	}
	if n := len(findMentions(strings.Join(words, " "))); n != maxMentionsPerMessage {
		t.Fatalf("findMentions capped at %d, got %d", maxMentionsPerMessage, n)
	}
}

func TestExpandFileMentionsAttachesContent(t *testing.T) {
	mentionTestDir(t)
	writeFile(t, "hello.txt", "hello world\n")
	writeFile(t, "sub/code.go", "package sub\n")

	block := expandFileMentions("explain @hello.txt and @sub/code.go please")
	for _, want := range []string{
		"===== hello.txt =====", "```\nhello world\n```",
		"===== sub/code.go =====", "package sub",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestExpandFileMentionsNoMentions(t *testing.T) {
	if got := expandFileMentions("just a plain message about files"); got != "" {
		t.Fatalf("expected empty block, got %q", got)
	}
	if got := expandFileMentions("thanks @user"); got != "" {
		t.Fatalf("handles must not expand, got %q", got)
	}
}

func TestExpandFileMentionsMissingAndUnreadable(t *testing.T) {
	mentionTestDir(t)
	block := expandFileMentions("look at @ghost.go")
	if !strings.Contains(block, "===== ghost.go =====") || !strings.Contains(block, "[not attached:") {
		t.Fatalf("missing file should leave an inline note:\n%s", block)
	}
}

func TestExpandFileMentionsBinarySkipped(t *testing.T) {
	mentionTestDir(t)
	if err := os.WriteFile("bin.dat", []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	block := expandFileMentions("open @bin.dat")
	if !strings.Contains(block, "binary file, skipped") {
		t.Fatalf("binary file should be noted as skipped:\n%s", block)
	}
}

func TestExpandFileMentionsJailEscape(t *testing.T) {
	dir := mentionTestDir(t)
	// A file just outside the workspace must not be read through a mention.
	writeFile(t, filepath.Join(dir, "..", filepath.Base(dir)+"-escape.txt"), "secret\n")
	escape := "../" + filepath.Base(dir) + "-escape.txt"
	block := expandFileMentions("show @" + escape)
	if strings.Contains(block, "secret") {
		t.Fatalf("jail escape read a file outside the workspace:\n%s", block)
	}
	if !strings.Contains(block, "[not attached:") {
		t.Fatalf("escape should leave an inline error note:\n%s", block)
	}
}

func TestExpandFileMentionsTruncatesLargeFile(t *testing.T) {
	mentionTestDir(t)
	writeFile(t, "big.txt", strings.Repeat("a", mentionMaxFileBytes+4096))
	block := expandFileMentions("summarize @big.txt")
	if !strings.Contains(block, "[truncated after") {
		t.Fatalf("large file should note truncation:\n%.200s...", block)
	}
	if strings.Contains(block, strings.Repeat("a", mentionMaxFileBytes+1)) {
		t.Fatal("content exceeded the per-file cap")
	}
}

func TestExpandFileMentionsTotalBudget(t *testing.T) {
	mentionTestDir(t)
	// Six files at the per-file cap exceed the 4x total budget; the later ones
	// must degrade to notes instead of flooding the turn.
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt"} {
		writeFile(t, name, strings.Repeat(name[:1], mentionMaxFileBytes))
	}
	block := expandFileMentions("@a.txt @b.txt @c.txt @d.txt @e.txt @f.txt")
	if !strings.Contains(block, "budget exhausted") {
		t.Fatalf("expected budget-exhausted notes:\n%.400s", block)
	}
}

func TestMentionTokenAtEnd(t *testing.T) {
	prefix, start, ok := mentionTokenAtEnd("fix @tui/key")
	if !ok || prefix != "tui/key" || start != len("fix @") {
		t.Fatalf("mentionTokenAtEnd = (%q, %d, %v)", prefix, start, ok)
	}
	if _, _, ok := mentionTokenAtEnd("no mention here"); ok {
		t.Fatal("plain text should not trigger completion")
	}
	if p, _, ok := mentionTokenAtEnd("@"); !ok || p != "" {
		t.Fatal("bare @ should open completion with an empty prefix")
	}
	if _, _, ok := mentionTokenAtEnd("mail me at user@example.com"); ok {
		t.Fatal("email-like token should not trigger completion")
	}
}

func TestMatchMentionCandidates(t *testing.T) {
	files := []string{"README.md", "docs/README.md", "main.go", "cmd/read.go"}
	got := matchMentionCandidates(files, "read", 10)
	// Prefix matches first, then substring matches in walk order; case-insensitive.
	if len(got) != 3 || got[0] != "README.md" || got[1] != "docs/README.md" || got[2] != "cmd/read.go" {
		t.Fatalf("match order: %v", got)
	}
	if got := matchMentionCandidates(files, "README.md", 10); len(got) != 0 {
		t.Fatalf("fully-typed path should close the menu, got %v", got)
	}
	if got := matchMentionCandidates(files, "", 2); len(got) != 2 {
		t.Fatalf("limit not applied: %v", got)
	}
}

func TestWalkWorkspaceFiles(t *testing.T) {
	dir := mentionTestDir(t)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "src", "a.go"), "package src\n")
	writeFile(t, filepath.Join(dir, "node_modules", "x.js"), "x\n")
	writeFile(t, filepath.Join(dir, "debug.log"), "log\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")

	got := walkWorkspaceFiles(dir, 100)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "main.go") || !strings.Contains(joined, filepath.Join("src", "a.go")) {
		t.Errorf("walk missed real files: %v", got)
	}
	if strings.Contains(joined, "node_modules") {
		t.Errorf("walk should skip node_modules: %v", got)
	}
	if strings.Contains(joined, "debug.log") {
		t.Errorf("walk should respect .gitignore: %v", got)
	}
}

// Tab on an open @menu completes the highlighted path into the token, leaving
// the rest of the message alone.
func TestMentionCompletionTabFillsPath(t *testing.T) {
	mentionTestDir(t)
	writeFile(t, "README.md", "# readme\n")

	m := typeKeys(t, newSized(t), "check @read")
	if !m.mentionVisible || len(m.mentionSuggestions) == 0 || m.mentionSuggestions[0] != "README.md" {
		t.Fatalf("expected README.md completion, visible=%v got %v", m.mentionVisible, m.mentionSuggestions)
	}
	m = press(t, m, tea.KeyTab, 0)
	if got := m.input.Value(); got != "check @README.md" {
		t.Fatalf("tab completed to %q, want %q", got, "check @README.md")
	}
	if m.mentionVisible {
		t.Fatal("menu should close after completing")
	}
}

// Enter on an open @menu accepts the completion instead of submitting, like
// the slash menu; the send happens on the next Enter.
func TestMentionCompletionEnterAcceptsBeforeSubmit(t *testing.T) {
	mentionTestDir(t)
	writeFile(t, "README.md", "# readme\n")

	m := typeKeys(t, newSized(t), "@read")
	m = press(t, m, tea.KeyEnter, 0)
	if got := m.input.Value(); got != "@README.md" {
		t.Fatalf("enter should accept the completion, input = %q", got)
	}
	if len(m.history) != 0 {
		t.Fatal("enter with the menu open must not submit")
	}
}

// The outgoing turn carries the expanded file contents while history keeps
// the message as typed, so the transcript shows the short form.
func TestSubmitAttachesMentionBlock(t *testing.T) {
	mentionTestDir(t)
	writeFile(t, "hello.txt", "hello world\n")

	m := newSized(t)
	m.modelName = "test-model"
	m.routeDeclines = routeMaxDeclines // no escalation offer in tests
	m.input.SetValue("explain @hello.txt")
	m.submit()

	if len(m.history) == 0 || m.history[len(m.history)-1].Content != "explain @hello.txt" {
		t.Fatalf("history should keep the typed message, got %+v", m.history)
	}
	if !strings.Contains(m.mentionBlock, "===== hello.txt =====") || !strings.Contains(m.mentionBlock, "hello world") {
		t.Fatalf("mention block not attached:\n%s", m.mentionBlock)
	}
	if !strings.Contains(m.buildDynamicContext(""), m.mentionBlock) {
		t.Fatal("mention block missing from the dynamic context sent to the model")
	}
	if m.stream != nil && m.stream.cancel != nil {
		m.stream.cancel() // don't leak the dial-out from startStream
	}
}
