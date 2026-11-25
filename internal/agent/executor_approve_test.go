package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/tools"
)

func destructiveRegistry(ran *int) *tools.Registry {
	r := tools.NewRegistry()
	r.Register(tools.Tool{
		Function: tools.Function{Name: "poke", Parameters: tools.Schema{Type: "object"}},
		Policy:   tools.ToolPolicy{Destructive: true},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			*ran++
			return "poked", nil
		},
	})
	return r
}

func pokeCall() tools.ToolCall {
	return tools.ToolCall{Function: tools.ToolCallFunction{Name: "poke", Arguments: json.RawMessage(`{}`)}}
}

func TestExecuteApproveHook(t *testing.T) {
	cases := []struct {
		name    string
		approve func(context.Context, ApprovalRequest) (bool, error)
		wantRun int
		wantErr string
	}{
		{"nil refuses ask", nil, 0, "refused without a user"},
		{"approved runs", func(context.Context, ApprovalRequest) (bool, error) { return true, nil }, 1, ""},
		{"declined refuses", func(context.Context, ApprovalRequest) (bool, error) { return false, nil }, 0, "declined"},
		{"error refuses", func(context.Context, ApprovalRequest) (bool, error) { return false, errors.New("boom") }, 0, "approval failed: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := 0
			ex := Executor{Registry: destructiveRegistry(&ran), NonInteractive: true, Approve: tc.approve, state: &approvalState{}}
			if tc.approve == nil {
				// Without a hook the run is auto mode: an in-workspace
				// destructive call proceeds, so force an Ask via a rule.
				ex.Permissions = []tools.PermissionRule{{Tool: "poke", Effect: tools.PermissionAsk}}
			}
			ev := ex.Execute(context.Background(), pokeCall())
			if ran != tc.wantRun {
				t.Fatalf("handler ran %d times, want %d", ran, tc.wantRun)
			}
			if tc.wantErr == "" && ev.Err != nil {
				t.Fatalf("unexpected error %v", ev.Err)
			}
			if tc.wantErr != "" && (ev.Err == nil || !strings.Contains(ev.Err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want containing %q", ev.Err, tc.wantErr)
			}
		})
	}
}

// With a hook installed the policy is Auto=false: a plain destructive call
// asks even with no rules, and the request carries the call and reason.
func TestExecuteApproveReceivesRequest(t *testing.T) {
	ran := 0
	var got ApprovalRequest
	ex := Executor{Registry: destructiveRegistry(&ran), NonInteractive: true, state: &approvalState{},
		Approve: func(_ context.Context, req ApprovalRequest) (bool, error) { got = req; return true, nil }}
	ex.Execute(context.Background(), pokeCall())
	if got.Call.Function.Name != "poke" || got.Reason == "" || !got.Policy.Destructive {
		t.Fatalf("request = %+v", got)
	}
}

// A deny rule still wins; the hook is never consulted.
func TestExecuteApproveNotCalledOnDeny(t *testing.T) {
	ran := 0
	ex := Executor{Registry: destructiveRegistry(&ran), NonInteractive: true, state: &approvalState{},
		Permissions: []tools.PermissionRule{{Tool: "poke", Effect: tools.PermissionDeny}},
		Approve:     func(context.Context, ApprovalRequest) (bool, error) { t.Fatal("hook called on deny"); return true, nil }}
	ex.Execute(context.Background(), pokeCall())
	if ran != 0 {
		t.Fatal("denied call ran")
	}
}
