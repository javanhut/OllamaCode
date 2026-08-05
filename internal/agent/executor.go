package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

// ExecutionEvent is the shared observable outcome emitted by TUI, headless,
// eval, and tracing callers for every attempted tool call.
type ExecutionEvent struct {
	Call            tools.ToolCall
	Result          string
	Err             error
	ArgumentFailure bool
	RepairAttempted bool
	RepairSucceeded bool
	Duration        time.Duration
}

// Executor is the single tool-dispatch implementation used by interactive and
// headless agent entry points. UI-specific permission checks happen before it;
// validation, timeouts, panic recovery, argument repair, and result envelopes
// happen here.
type Executor struct {
	Registry          *tools.Registry
	Host              ChatClient
	Model             string
	NumCtx            int
	Before            func(tools.ToolCall)
	Observe           func(ExecutionEvent)
	StructuredResults *bool // nil/true=envelopes; false is eval-only legacy comparison
}

func (e Executor) Execute(ctx context.Context, call tools.ToolCall) ExecutionEvent {
	started := time.Now()
	event := ExecutionEvent{Call: call}
	defer func() { event.Duration = time.Since(started) }()
	if e.Registry == nil {
		event.Err = fmt.Errorf("tool registry is unavailable")
		event.Result = tools.EncodeToolFailure("tool execution failed", event.Err.Error(), false)
		return event
	}
	call.Function.Arguments = tools.SalvageJSON(call.Function.Arguments)
	event.Call = call
	if e.Before != nil {
		e.Before(call)
	}

	out, err := invokeWithTimeout(ctx, e.Registry, call, ToolTimeout(call))
	if err != nil && tools.ShouldFormatRepair(call, err) && e.Host != nil {
		event.ArgumentFailure = true
		event.RepairAttempted = true
		if fixed, ok := RepairArgsViaFormat(ctx, e.Host, e.Registry, e.Model, e.NumCtx, call); ok {
			call.Function.Arguments = fixed
			event.Call = call
			out, err = invokeWithTimeout(ctx, e.Registry, call, ToolTimeout(call))
			if err == nil {
				event.RepairSucceeded = true
			}
		}
	}
	event.Err = err
	structured := e.StructuredResults == nil || *e.StructuredResults
	if err != nil && structured {
		hint := tools.RepairHint(call, err)
		event.Result = tools.EncodeToolFailure(call.Function.Name+" failed", hint, isRetryableToolError(err))
	} else if err == nil && structured {
		event.Result = tools.EncodeToolSuccess(call.Function.Name, out)
	} else if err != nil {
		event.Result = tools.RepairHint(call, err)
	} else {
		event.Result = out
	}
	if e.Observe != nil {
		event.Duration = time.Since(started)
		e.Observe(event)
	}
	return event
}

func isRetryableToolError(err error) bool {
	if err == nil {
		return false
	}
	// Validation, missing paths, and transient command failures are actionable;
	// an unavailable registry/handler is not.
	return !containsAny(err.Error(), "has no handler", "registry is unavailable")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
