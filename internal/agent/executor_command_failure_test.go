package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

// A command that runs and exits nonzero must report ok:false while keeping its
// output — the old envelope said ok:true over a traceback.
func TestExecuteMarksNonZeroExitAsFailure(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(tools.RunShellTool())
	ev := Executor{Registry: reg}.Execute(context.Background(), tools.ToolCall{
		Function: tools.ToolCallFunction{Name: "run_shell", Arguments: json.RawMessage(`{"command":"echo boom >&2; exit 3"}`)},
	})

	if tools.ToolResultOK(ev.Result) {
		t.Fatalf("failed command reported ok: %s", ev.Result)
	}
	if ev.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", ev.ExitCode)
	}
	// The tool itself worked, so nothing should look like a broken tool.
	if ev.Err != nil {
		t.Fatalf("Err = %v, want nil for a command-level failure", ev.Err)
	}
	env, ok := tools.DecodeToolResult(ev.Result)
	if !ok {
		t.Fatalf("undecodable envelope: %s", ev.Result)
	}
	if !strings.Contains(strings.Join(env.Evidence, "\n"), "boom") {
		t.Fatalf("command output lost: %#v", env.Evidence)
	}
	if !strings.Contains(env.Summary, "exited 3") {
		t.Fatalf("summary = %q, want the exit code", env.Summary)
	}
}

func TestExecuteKeepsZeroExitSuccessful(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(tools.RunShellTool())
	ev := Executor{Registry: reg}.Execute(context.Background(), tools.ToolCall{
		Function: tools.ToolCallFunction{Name: "run_shell", Arguments: json.RawMessage(`{"command":"echo fine"}`)},
	})
	if !tools.ToolResultOK(ev.Result) || ev.ExitCode != 0 || ev.Err != nil {
		t.Fatalf("successful command misreported: ok=%v exit=%d err=%v", tools.ToolResultOK(ev.Result), ev.ExitCode, ev.Err)
	}
}
