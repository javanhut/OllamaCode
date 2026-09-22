package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

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
