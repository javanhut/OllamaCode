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

// ApprovalRequest describes an Ask the executor hands to Executor.Approve.
type ApprovalRequest struct {
	Call   tools.ToolCall
	Policy tools.ToolPolicy
	Reason string
	// Untrusted: web/MCP content entered this turn. Protected: the call
	// targets agent/editor config or git hooks. Remembered "allow always"
	// grants must not waive either.
	Untrusted bool
	Protected bool
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
	// Permissions are the user's configured allow/ask/deny rules. Only deny is
	// enforced here: allow and ask are answers to "should the UI prompt?", which
	// is the caller's question, but a deny has to hold for every caller — a rule
	// that stopped a write in the TUI and not in a spawned subagent would be a
	// hole in exactly the boundary it was written to close.
	Permissions []tools.PermissionRule
	// NonInteractive: nobody can answer a prompt (headless, subagents, eval).
	// The full approval policy (Decide) is enforced here and an Ask is
	// refused. Interactive callers run Decide in their own preflight and only
	// a Deny is enforced here, after the human has answered.
	NonInteractive bool
	// Approve, when set on a NonInteractive executor, answers an Ask instead of
	// the executor refusing it (the ACP adapter forwards it to the editor). It
	// also switches Decide to Auto=false: every destructive call asks, since
	// someone is there to answer. A false answer or an error refuses the call.
	// Nil keeps the refuse-on-Ask default.
	Approve func(ctx context.Context, req ApprovalRequest) (bool, error)
	state   *approvalState // shared across copies of one run's executor
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
	call.Function.Arguments = tools.UnwrapQuotedArgs(tools.SalvageJSON(call.Function.Arguments))
	event.Call = call
	// Salvage first, then check: a deny rule matches on the call's real
	// arguments, not on whatever malformed JSON the model emitted around them.
	policy := tools.PolicyForName(call.Function.Name)
	if tool, ok := e.Registry.Lookup(call.Function.Name); ok {
		policy = tool.Policy
	}
	decision, reason := Decide(ApprovalInput{
		Call: call, Policy: policy, ConfigRules: e.Permissions,
		Auto: e.Approve == nil, Untrusted: e.state != nil && e.state.untrusted.Load(),
	})
	approveFailed := ""
	if decision == Ask && e.NonInteractive && e.Approve != nil {
		ok, err := e.Approve(ctx, ApprovalRequest{Call: call, Policy: policy, Reason: reason,
			Untrusted: e.state != nil && e.state.untrusted.Load(), Protected: TouchesProtectedPath(call)})
		switch {
		case ok && err == nil:
			decision = Allow
		case err != nil:
			approveFailed = "approval failed: " + err.Error()
		default:
			approveFailed = "the user declined this action"
		}
	}
	if approveFailed != "" {
		reason = approveFailed
		event.Err = fmt.Errorf("%s", reason)
		event.Result = tools.EncodeToolFailure(reason, "Do NOT retry this call or work around it (including via run_shell). Continue without it, or tell the user what you wanted to do.", false)
		if e.Observe != nil {
			e.Observe(event)
		}
		return event
	}
	if decision == Deny || (decision == Ask && e.NonInteractive) {
		hint := "Do NOT retry this call or a minor variant — take a different approach, or tell the user which rule is in your way."
		if decision == Ask {
			reason = "refused without a user to approve it: " + reason
			hint = "This run cannot ask the user. Do NOT retry or work around it (including via run_shell); finish without this action and tell the user exactly what you wanted to do and why, so they can do it or add an allow rule to config.json."
		}
		event.Err = fmt.Errorf("%s", reason)
		event.Result = tools.EncodeToolFailure(reason, hint, false)
		if e.Observe != nil {
			e.Observe(event)
		}
		return event
	}
	defer func() {
		if e.state != nil && ResultIsUntrusted(call.Function.Name, policy, event.Result) {
			e.state.untrusted.Store(true)
		}
	}()
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
	if cmdFail, ok := errors.AsType[*tools.CommandFailure](err); ok {
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
