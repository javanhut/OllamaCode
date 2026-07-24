package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

// toolTimeout bounds a single tool call so a slow or hung tool can't stall the
// whole agent. run_shell honors the model's timeout_sec (capped); network-ish
// tools get longer; everything else a modest default.
func toolTimeout(call tools.ToolCall) time.Duration {
	switch call.Function.Name {
	case "run_shell":
		var a struct {
			TimeoutSec float64 `json:"timeout_sec"`
		}
		_ = json.Unmarshal(call.Function.Arguments, &a)
		t := 30 * time.Second
		if a.TimeoutSec > 0 {
			t = time.Duration(a.TimeoutSec * float64(time.Second))
		}
		if t > 300*time.Second {
			t = 300 * time.Second
		}
		return t + 5*time.Second
	case "web_search", "web_fetch", "code_index", "semantic_search":
		return 2 * time.Minute
	default:
		return 90 * time.Second
	}
}

// invokeWithTimeout runs one tool call with a deadline and panic recovery, so a
// hanging or panicking handler surfaces as an error the model can react to
// instead of killing the run.
func invokeWithTimeout(ctx context.Context, reg *tools.Registry, call tools.ToolCall, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{"", fmt.Errorf("tool %q panicked: %v", call.Function.Name, r)}
			}
		}()
		out, err := reg.Invoke(cctx, call)
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		return r.out, r.err
	case <-cctx.Done():
		// ponytail: the handler goroutine may linger until it notices ctx cancel —
		// acceptable; well-behaved handlers respect the passed context.
		return "", fmt.Errorf("tool %q timed out after %s; do not retry the same arguments — narrow the command or try another approach", call.Function.Name, timeout)
	}
}
