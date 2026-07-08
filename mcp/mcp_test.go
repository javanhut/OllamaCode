package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(ReadFileTool())

	defs := r.Definitions()
	if len(defs) != 1 {
		t.Errorf("expected 1 tool definition, got %d", len(defs))
	}
	if defs[0].Function.Name != "read_file" {
		t.Errorf("expected tool name 'read_file', got %q", defs[0].Function.Name)
	}

	ctx := context.Background()
	call := ToolCall{
		Function: ToolCallFunction{
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path": "nonexistent"}`),
		},
	}
	_, err := r.Invoke(ctx, call)
	if err == nil {
		t.Error("expected error for nonexistent file, got nil")
	}
}

func TestRunShellToolReturnsOutput(t *testing.T) {
	r := NewRegistry()
	r.Register(RunShellTool())

	resp, err := r.Invoke(context.Background(), ToolCall{
		Function: ToolCallFunction{
			Name:      "run_shell",
			Arguments: json.RawMessage(`{"command":"printf hello"}`),
		},
	})
	if err != nil {
		t.Fatalf("run_shell failed: %v", err)
	}
	if resp != "hello" {
		t.Fatalf("expected output, got %q", resp)
	}
}

func TestRunShellToolTimeout(t *testing.T) {
	r := NewRegistry()
	r.Register(RunShellTool())

	start := time.Now()
	resp, err := r.Invoke(context.Background(), ToolCall{
		Function: ToolCallFunction{
			Name:      "run_shell",
			Arguments: json.RawMessage(`{"command":"sleep 5","timeout_sec":0.2}`),
		},
	})
	if err != nil {
		t.Fatalf("run_shell failed: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
	if !strings.Contains(resp, "[timed out after 200ms]") {
		t.Fatalf("expected timeout marker, got %q", resp)
	}
}

func TestRunShellToolTimeoutKillsBackgroundChildren(t *testing.T) {
	r := NewRegistry()
	r.Register(RunShellTool())

	start := time.Now()
	resp, err := r.Invoke(context.Background(), ToolCall{
		Function: ToolCallFunction{
			Name:      "run_shell",
			Arguments: json.RawMessage(`{"command":"sleep 5 & wait","timeout_sec":0.2}`),
		},
	})
	if err != nil {
		t.Fatalf("run_shell failed: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("background child kept command stuck for %s; response %q", time.Since(start), resp)
	}
	if !strings.Contains(resp, "[timed out after 200ms]") {
		t.Fatalf("expected timeout marker, got %q", resp)
	}
}

func TestWriteAndReadFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	r := NewRegistry()
	r.Register(WriteFileTool())
	r.Register(ReadFileTool())

	path := filepath.Join(tmpDir, "test.txt")
	content := "hello world"

	// Write
	writeArgs, _ := json.Marshal(map[string]string{
		"path":    path,
		"content": content,
	})
	_, err = r.Invoke(ctx, ToolCall{
		Function: ToolCallFunction{Name: "write_file", Arguments: writeArgs},
	})
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}

	// Read
	readArgs, _ := json.Marshal(map[string]string{
		"path": path,
	})
	resp, err := r.Invoke(ctx, ToolCall{
		Function: ToolCallFunction{Name: "read_file", Arguments: readArgs},
	})
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	// Whole-file reads are line-numbered (1-indexed, tab-separated) so the model
	// has stable coordinates for edit_file.
	if want := "1\t" + content; resp != want {
		t.Errorf("expected %q, got %q", want, resp)
	}
}

func TestReadFileNumbersAndTruncates(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-read-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := ReadFileTool()

	// Whole-file read: every line numbered, 1-indexed. (trailing "\n" -> empty 4th line)
	args, _ := json.Marshal(map[string]any{"path": path})
	out, err := tool.Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1\talpha\n2\tbeta\n3\tgamma\n4\t"; out != want {
		t.Fatalf("read_file =\n%q\nwant\n%q", out, want)
	}

	// Tiny byte budget truncates and still emits at least the first line + a note.
	args, _ = json.Marshal(map[string]any{"path": path, "max_bytes": 1})
	out, err = tool.Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "1\talpha\n") || !strings.Contains(out, "truncated at line") {
		t.Fatalf("expected first line + truncation note, got %q", out)
	}
}

func TestEditFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	r := NewRegistry()
	r.Register(WriteFileTool())
	r.Register(EditFileTool())
	r.Register(ReadFileTool())

	path := filepath.Join(tmpDir, "edit.txt")
	initial := "line 1\nline 2\nline 3"

	// Write initial
	writeArgs, _ := json.Marshal(map[string]string{
		"path":    path,
		"content": initial,
	})
	r.Invoke(ctx, ToolCall{Function: ToolCallFunction{Name: "write_file", Arguments: writeArgs}})

	// Edit
	editArgs, _ := json.Marshal(map[string]any{
		"path":       path,
		"old_string": "line 2",
		"new_string": "line two",
	})
	_, err = r.Invoke(ctx, ToolCall{
		Function: ToolCallFunction{Name: "edit_file", Arguments: editArgs},
	})
	if err != nil {
		t.Fatalf("edit_file failed: %v", err)
	}

	// Verify the file content directly (read_file's output is line-numbered).
	got, _ := os.ReadFile(path)
	expected := "line 1\nline two\nline 3"
	if string(got) != expected {
		t.Errorf("expected %q, got %q", expected, string(got))
	}
}

func TestEditFile_LineRange(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	r := NewRegistry()
	r.Register(WriteFileTool())
	r.Register(EditFileTool())
	r.Register(ReadFileTool())

	path := filepath.Join(tmpDir, "edit_range.txt")
	initial := "one\ntwo\nthree\nfour\nfive"

	// Write initial
	writeArgs, _ := json.Marshal(map[string]string{
		"path":    path,
		"content": initial,
	})
	r.Invoke(ctx, ToolCall{Function: ToolCallFunction{Name: "write_file", Arguments: writeArgs}})

	// Edit line 3 to 4
	editArgs, _ := json.Marshal(map[string]any{
		"path":       path,
		"start_line": 3,
		"end_line":   4,
		"new_string": "THREE\nFOUR",
	})
	_, err = r.Invoke(ctx, ToolCall{
		Function: ToolCallFunction{Name: "edit_file", Arguments: editArgs},
	})
	if err != nil {
		t.Fatalf("edit_file line range failed: %v", err)
	}

	// Verify the file content directly (read_file's output is line-numbered).
	got, _ := os.ReadFile(path)
	expected := "one\ntwo\nTHREE\nFOUR\nfive"
	if string(got) != expected {
		t.Errorf("expected %q, got %q", expected, string(got))
	}
}

func TestGrep(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	r := NewRegistry()
	r.Register(WriteFileTool())
	r.Register(GrepTool())

	path := filepath.Join(tmpDir, "search.txt")
	content := "foo\nbar\nbaz"
	writeArgs, _ := json.Marshal(map[string]string{"path": path, "content": content})
	r.Invoke(ctx, ToolCall{Function: ToolCallFunction{Name: "write_file", Arguments: writeArgs}})

	// Search
	grepArgs, _ := json.Marshal(map[string]string{
		"pattern": "bar",
		"path":    path,
	})
	resp, err := r.Invoke(ctx, ToolCall{
		Function: ToolCallFunction{Name: "grep", Arguments: grepArgs},
	})
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	if !strings.Contains(resp, "bar") {
		t.Errorf("expected response to contain 'bar', got %q", resp)
	}
}

func TestWriteFileEmitsDiff(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcp-diff-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	tool := WriteFileTool()
	path := filepath.Join(tmpDir, "f.txt")

	write := func(content string) string {
		args, _ := json.Marshal(map[string]string{"path": path, "content": content})
		out, err := tool.Handler(ctx, args)
		if err != nil {
			t.Fatalf("write_file failed: %v", err)
		}
		return out
	}

	write("line one\nline two\n")
	out := write("line one\nline CHANGED\n")

	if !strings.Contains(out, "--- "+path) || !strings.Contains(out, "+++ "+path) {
		t.Fatalf("overwrite result missing diff header:\n%s", out)
	}
	if !strings.Contains(out, "+line CHANGED") || !strings.Contains(out, "-line two") {
		t.Fatalf("diff missing changed lines:\n%s", out)
	}
}
