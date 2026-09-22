package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// A handler that shells out returns the command's output alongside its error so
// the model can read the reason. The executor used to drop that string and hand
// back a bare "error: exit status 1" — two turns of the ocode_test session went
// to git_status/git_log failing with no stated cause.
func TestExecuteKeepsHandlerOutputOnError(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:       "explodes",
			Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{}},
		},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "not an Ivaldi repository (or any parent)", errors.New("exit status 1")
		},
	})
	ev := Executor{Registry: reg}.Execute(context.Background(), tools.ToolCall{
		Function: tools.ToolCallFunction{Name: "explodes", Arguments: json.RawMessage(`{}`)},
	})

	if tools.ToolResultOK(ev.Result) {
		t.Fatalf("failed handler reported ok: %s", ev.Result)
	}
	env, ok := tools.DecodeToolResult(ev.Result)
	if !ok {
		t.Fatalf("undecodable envelope: %s", ev.Result)
	}
	// The diagnostic rides evidence, not the hint: that is the encoder path with
	// line splitting and, for a large one, a spill file. The hint keeps the
	// repair guidance and the underlying error.
	if !strings.Contains(strings.Join(env.Evidence, "\n"), "not an Ivaldi repository") {
		t.Fatalf("handler diagnostic lost, evidence = %q", env.Evidence)
	}
	if !strings.Contains(env.Hint, "exit status 1") {
		t.Fatalf("underlying error lost, hint = %q", env.Hint)
	}
}

// A large diagnostic from a plain (out, err) handler must be recoverable, the
// same as one from a handler returning *CommandFailure. It used to go into the
// hint, which is encoded by the one path that neither splits evidence nor
// spills, so the middle was destroyed with no locator.
func TestExecuteSpillsLargeHandlerOutput(t *testing.T) {
	marker := "THE-MIDDLE-OF-THE-DIAGNOSTIC"
	big := strings.Repeat("conflict line\n", 2000) + marker + "\n" + strings.Repeat("more conflict\n", 2000)

	reg := tools.NewRegistry()
	reg.Register(tools.Tool{
		Type: "function",
		Function: tools.Function{
			Name:       "explodes_loudly",
			Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{}},
		},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return big, errors.New("exit status 1")
		},
	})
	t.Cleanup(tools.CleanupSpills)

	ev := Executor{Registry: reg}.Execute(context.Background(), tools.ToolCall{
		Function: tools.ToolCallFunction{Name: "explodes_loudly", Arguments: json.RawMessage(`{}`)},
	})

	env, ok := tools.DecodeToolResult(ev.Result)
	if !ok {
		t.Fatalf("undecodable envelope: %s", ev.Result)
	}
	if !env.Truncated || env.SpillPath == "" {
		t.Fatalf("large diagnostic was not spilled: truncated=%v path=%q", env.Truncated, env.SpillPath)
	}
	saved, err := os.ReadFile(env.SpillPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), marker) {
		t.Error("spill file does not hold the elided middle")
	}
}
