package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/internal/lsp"
)

// invokeMultiEdit runs multi_edit through Registry.Invoke — the real path,
// including NormalizeArgs — against a file in a jailed temp workspace.
func invokeMultiEdit(t *testing.T, content, args string) (path, out string, err error) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	path = filepath.Join(root, "f.txt")
	if werr := os.WriteFile(path, []byte(content), 0o644); werr != nil {
		t.Fatal(werr)
	}
	r := NewRegistry()
	r.Register(MultiEditTool())
	out, err = r.Invoke(context.Background(), ToolCall{Function: ToolCallFunction{
		Name: "multi_edit", Arguments: []byte(strings.ReplaceAll(args, "PATH", path)),
	}})
	return path, out, err
}

func TestMultiEditAppliesInOrder(t *testing.T) {
	// The second edit matches text the first one produced: order matters.
	path, out, err := invokeMultiEdit(t, "alpha\nbeta\ngamma\n",
		`{"path":"PATH","edits":[{"old_string":"alpha","new_string":"ALPHA"},{"old_string":"ALPHA\nbeta","new_string":"ALPHA\nBETA"},{"old_string":"gamma","new_string":"GAMMA"}]}`)
	if err != nil {
		t.Fatalf("multi_edit failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "ALPHA\nBETA\nGAMMA\n" {
		t.Fatalf("unexpected content %q", got)
	}
	if !strings.Contains(out, "applied 3 edit(s), 3 replacement(s)") {
		t.Fatalf("unexpected result: %s", out)
	}
}

func TestMultiEditIsAtomic(t *testing.T) {
	path, _, err := invokeMultiEdit(t, "one\ntwo\n",
		`{"path":"PATH","edits":[{"old_string":"one","new_string":"ONE"},{"old_string":"missing text that is not in the file anywhere","new_string":"x"}]}`)
	if err == nil {
		t.Fatal("expected failure when an edit does not match")
	}
	if !strings.Contains(err.Error(), "edit 2 of 2 failed") || !strings.Contains(err.Error(), "No changes were written") {
		t.Fatalf("error should name the failing edit: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "one\ntwo\n" {
		t.Fatalf("file must be untouched after a failed multi_edit, got %q", got)
	}
}

func TestMultiEditAcceptsStringifiedEdits(t *testing.T) {
	path, _, err := invokeMultiEdit(t, "a = 1\nb = 2\n",
		`{"path":"PATH","edits":"[{\"old_string\":\"a = 1\",\"new_string\":\"a = 10\"},{\"old_string\":\"b = 2\",\"new_string\":\"b = 20\"}]"}`)
	if err != nil {
		t.Fatalf("stringified edits should be accepted: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a = 10\nb = 20\n" {
		t.Fatalf("unexpected content %q", got)
	}
}

func TestMultiEditRejectsSyntaxBreak(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(root, "x.json")
	if err := os.WriteFile(path, []byte(`{"a": 1, "b": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, err := resolveMultiEdit(multiEditArgs{Path: path, Edits: []multiEditItem{
		{OldString: `"a": 1,`, NewString: `"a": 1,,`},
	}})
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("expected syntax-guard rejection, got %v", err)
	}
}

func TestPreviewMultiEditMatchesHandler(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(root, "p.txt")
	if err := os.WriteFile(path, []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, ok := PreviewMultiEdit(path, []byte(`{"path":"`+path+`","edits":"[{\"old_string\":\"y\",\"new_string\":\"z\"}]"}`))
	if !ok || !strings.Contains(diff, "-y") || !strings.Contains(diff, "+z") {
		t.Fatalf("unexpected preview (ok=%v): %s", ok, diff)
	}
	if got, _ := os.ReadFile(path); string(got) != "x\ny\n" {
		t.Fatal("preview must not write")
	}
}

func TestFormatEditDiagnostics(t *testing.T) {
	if formatEditDiagnostics("a.go", nil) != "" {
		t.Fatal("no errors should render nothing")
	}
	var errs []lsp.Diagnostic
	for i := range 22 {
		var d lsp.Diagnostic
		d.Range.Start.Line = i
		d.Range.Start.Character = 4
		d.Message = " undefined: foo "
		errs = append(errs, d)
	}
	out := formatEditDiagnostics("a.go", errs)
	for _, want := range []string{
		"LSP errors detected in this file, please fix:",
		`<diagnostics file="a.go">`,
		"ERROR [1:5] undefined: foo",
		"ERROR [20:5]",
		"... and 2 more",
		"</diagnostics>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ERROR [21:5]") {
		t.Fatal("output should be capped at 20 diagnostics")
	}
}

// Without ConfigureLSP(true, ...) — the unit-test default — post-edit
// diagnostics must be a no-op, so an edit never spawns or waits on a server.
func TestPostEditDiagnosticsOffByDefault(t *testing.T) {
	ConfigureLSP(false, ".", nil)
	t.Cleanup(func() { ConfigureLSP(false, ".", nil) })
	if got := postEditDiagnostics(context.Background(), "main.go"); got != "" {
		t.Fatalf("expected no-op, got %q", got)
	}
}
