package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

func TestExecuteAttachesReadImage(t *testing.T) {
	root := t.TempDir()
	tools.SetWorkspaceRoot(root)
	t.Cleanup(func() { tools.SetWorkspaceRoot("") })
	t.Chdir(root)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	os.WriteFile(filepath.Join(root, "a.png"), buf.Bytes(), 0o644)

	reg := tools.NewRegistry()
	reg.Register(tools.ReadImageTool())
	exec := Executor{Registry: reg}
	call := func(path string) ExecutionEvent {
		return exec.Execute(context.Background(), tools.ToolCall{Function: tools.ToolCallFunction{
			Name: "read_image", Arguments: json.RawMessage(`{"path":"` + path + `"}`)}})
	}

	if ev := call("a.png"); ev.Err != nil || len(ev.Images) != 1 || !tools.ToolResultOK(ev.Result) {
		t.Fatalf("err=%v images=%d result=%s", ev.Err, len(ev.Images), ev.Result)
	}
	if ev := call("missing.png"); ev.Err == nil || len(ev.Images) != 0 {
		t.Fatalf("a missing image produced err=%v images=%d", ev.Err, len(ev.Images))
	}
}

// A failed match on a small file reaches the model as a failure that carries
// the file's current contents and the suggestion to rewrite it whole.
func TestExecuteEditFallbackCarriesFileContents(t *testing.T) {
	root := t.TempDir()
	tools.SetWorkspaceRoot(root)
	t.Cleanup(func() { tools.SetWorkspaceRoot("") })
	t.Chdir(root)
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nconst Marker = 42\n"), 0o644)

	reg := tools.NewRegistry()
	reg.Register(tools.EditFileTool())
	ev := Executor{Registry: reg}.Execute(context.Background(), tools.ToolCall{Function: tools.ToolCallFunction{
		Name: "edit_file", Arguments: json.RawMessage(`{"path":"a.go","old_string":"func Nowhere() {}","new_string":"x"}`)}})

	env, ok := tools.DecodeToolResult(ev.Result)
	if !ok || env.OK {
		t.Fatalf("want a failure envelope, got %s", ev.Result)
	}
	if !strings.Contains(env.Hint, "write_file") || !strings.Contains(strings.Join(env.Evidence, "\n"), "const Marker = 42") {
		t.Fatalf("hint=%q evidence=%q", env.Hint, env.Evidence)
	}
}
