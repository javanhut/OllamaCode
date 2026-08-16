package agent

import (
	"context"
	"errors"
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
	ExitCode        int // nonzero when a command ran and failed; Err stays nil
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

	out, err := invokeWithTimeout(ctx, e.Registry, call, tools.ToolCallTimeout(call))
	if err != nil && tools.ShouldFormatRepair(call, err) && e.Host != nil {
		event.ArgumentFailure = true
		event.RepairAttempted = true
		if fixed, ok := RepairArgsViaFormat(ctx, e.Host, e.Registry, e.Model, e.NumCtx, call); ok {
			call.Function.Arguments = fixed
			event.Call = call
			out, err = invokeWithTimeout(ctx, e.Registry, call, tools.ToolCallTimeout(call))
			if err == nil {
				event.RepairSucceeded = true
			}
		}
	}
	// A command that ran and exited nonzero is not a tool error: the output is
	// real evidence the model must read, and Err stays nil so failure counters
	// and dataset export keep meaning "the tool itself broke". Only the
	// envelope's ok flag reports the command's verdict.
	var cmdFail *tools.CommandFailure
	if errors.As(err, &cmdFail) {
		event.ExitCode = cmdFail.ExitCode
		if e.StructuredResults == nil || *e.StructuredResults {
			event.Result = tools.EncodeCommandFailure(call.Function.Name, cmdFail.Output, cmdFail.ExitCode)
		} else {
			event.Result = cmdFail.Output
		}
		if e.Observe != nil {
			event.Duration = time.Since(started)
			e.Observe(event)
		}
		return event
	}
	event.Err = err
	structured := e.StructuredResults == nil || *e.StructuredResults
	if err != nil && structured {
		hint := tools.RepairHint(call, err)
		// Handlers that shell out return the command's own output alongside the
		// error (`return string(out), err`) precisely so the model can read why
		// it failed. Dropping it left every git_* failure reading "error: exit
		// status 1" while the real answer — "not an Ivaldi repository", a
		// rejected push, a merge conflict — sat in `out`, so the model retried
		// blind instead of reporting the blocker.
		//
		// It rides the envelope's evidence rather than the hint, through the same
		// encoder every other result uses: concatenating it into the hint sent it
		// down the one path that skips evidence splitting and spilling, so a
		// 300KB merge conflict lost its middle with no spill_path to read back.
		if diag := strings.TrimSpace(out); diag != "" {
			event.Result = tools.EncodeToolFailureWithOutput(call.Function.Name+" failed", hint, isRetryableToolError(err), diag)
		} else {
			event.Result = tools.EncodeToolFailure(call.Function.Name+" failed", hint, isRetryableToolError(err))
		}
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
