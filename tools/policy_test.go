package tools

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDefaultRegistryPolicies(t *testing.T) {
	reg := DefaultRegistry()
	for _, tool := range reg.Definitions() {
		if tool.Policy.Modes == 0 {
			t.Errorf("tool %q has no mode policy", tool.Function.Name)
		}
		if tool.Policy.Timeout <= 0 {
			t.Errorf("tool %q has no timeout", tool.Function.Name)
		}
	}

	web, _ := reg.Lookup("web_search")
	if !web.Policy.Allows(ModeExplore) || !web.Policy.SmallModelSafe || !web.Policy.Network {
		t.Fatalf("web_search policy is incomplete: %+v", web.Policy)
	}
	write, _ := reg.Lookup("write_file")
	if write.Policy.Allows(ModeExplore) || !write.Policy.Destructive {
		t.Fatalf("write_file policy is unsafe: %+v", write.Policy)
	}
}

func TestUnknownToolPolicyIsConservative(t *testing.T) {
	p := PolicyForName("external_unclassified")
	if p.Allows(ModeExplore) || p.SmallModelSafe || !p.Destructive || !p.Network {
		t.Fatalf("unexpected external-tool default: %+v", p)
	}
	if p.Timeout != DefaultToolTimeout {
		t.Fatalf("expected default timeout, got %s", p.Timeout)
	}
}

func TestEveryPolicyHasTimeout(t *testing.T) {
	for name, p := range toolPolicies {
		if p.Timeout <= 0 {
			t.Errorf("tool %q has no timeout: a zero deadline arms context.WithTimeout(0)", name)
		}
	}
}

// TestToolCallTimeoutByClass replaces tui's TestToolCallTimeoutPolicy, which
// tested the name switch that used to live at the dispatch site.
func TestToolCallTimeoutByClass(t *testing.T) {
	tests := []struct {
		name string
		call ToolCall
		want time.Duration
	}{
		{"inspect", callOf("git_status", `{}`), inspectTimeout},
		{"mutate", callOf("write_file", `{}`), mutateTimeout},
		{"network", callOf("web_search", `{}`), networkTimeout},
		{"long running", callOf("spawn_subagent", `{}`), longTimeout},
		{"unclassified registered tool", callOf("get_project_tree", `{}`), DefaultToolTimeout},
		{"shell requested timeout gets cleanup grace", callOf("run_shell", `{"timeout_sec":1}`), time.Second + shellGrace},
		{"shell default", callOf("run_shell", `{}`), shellDefaultTimeout + shellGrace},
		{"shell request is capped", callOf("run_shell", `{"timeout_sec":9999}`), shellMaxTimeout + shellGrace},
		// git_show is a compat alias in nobody's registry: it resolves to the
		// conservative unknown-tool default now, not the old 30s inspect class.
		{"unregistered compat alias", callOf("git_show", `{}`), DefaultToolTimeout},
		{"unknown tools do not get long budget", callOf("custom_tool", `{}`), DefaultToolTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolCallTimeout(tt.call); got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

func callOf(name, args string) ToolCall {
	return ToolCall{Function: ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func permCall(name, args string) ToolCall {
	return ToolCall{Function: ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestEvaluatePermissionNoRules(t *testing.T) {
	if _, ok := EvaluatePermission(nil, permCall("write_file", `{"path":"a.go"}`)); ok {
		t.Fatal("empty ruleset must not match; callers keep their built-in behavior")
	}
}

func TestEvaluatePermissionDenyOutranksAllow(t *testing.T) {
	rules := []PermissionRule{
		{Tool: "*", Effect: PermissionAllow},
		{Tool: "write_file", Path: "secrets/**", Effect: PermissionDeny},
	}
	if effect, ok := EvaluatePermission(rules, permCall("write_file", `{"path":"secrets/keys.json"}`)); !ok || effect != PermissionDeny {
		t.Fatalf("deny must win regardless of rule order, got %q ok=%v", effect, ok)
	}
	if effect, ok := EvaluatePermission(rules, permCall("write_file", `{"path":"src/main.go"}`)); !ok || effect != PermissionAllow {
		t.Fatalf("path outside the deny subtree should stay allowed, got %q ok=%v", effect, ok)
	}
}

func TestEvaluatePermissionAskOutranksAllow(t *testing.T) {
	rules := []PermissionRule{
		{Tool: "git_*", Effect: PermissionAllow},
		{Tool: "git_push", Effect: PermissionAsk},
	}
	if effect, _ := EvaluatePermission(rules, permCall("git_push", `{}`)); effect != PermissionAsk {
		t.Fatalf("ask must outrank allow, got %q", effect)
	}
	if effect, _ := EvaluatePermission(rules, permCall("git_add", `{"path":"a.go"}`)); effect != PermissionAllow {
		t.Fatalf("glob should still allow siblings, got %q", effect)
	}
}

func TestEvaluatePermissionCommandPrefix(t *testing.T) {
	rules := []PermissionRule{{Tool: "run_shell", Cmd: "npm *", Effect: PermissionAllow}}
	// A trailing * spans separators: path.Match would miss this one.
	if _, ok := EvaluatePermission(rules, permCall("run_shell", `{"command":"npm run build --prefix ./web"}`)); !ok {
		t.Fatal("command prefix rule should match an argument containing a slash")
	}
	if _, ok := EvaluatePermission(rules, permCall("run_shell", `{"command":"rm -rf /"}`)); ok {
		t.Fatal("command prefix rule matched an unrelated command")
	}
}

func TestEvaluatePermissionBareNamePattern(t *testing.T) {
	rules := []PermissionRule{{Tool: "*", Path: "*.env", Effect: PermissionDeny}}
	if _, ok := EvaluatePermission(rules, permCall("write_file", `{"path":"config/prod.env"}`)); !ok {
		t.Fatal("a separator-free pattern should match the base name at any depth")
	}
}

// A resource-scoped rule must not match a call that carries no such resource,
// or "deny path:secrets/**" would silently deny unrelated toolless calls.
func TestEvaluatePermissionResourceRuleNeedsResource(t *testing.T) {
	rules := []PermissionRule{{Tool: "*", Path: "secrets/**", Effect: PermissionDeny}}
	if _, ok := EvaluatePermission(rules, permCall("git_status", `{}`)); ok {
		t.Fatal("path rule matched a call with no mutated paths")
	}
}

func TestPermissionRuleForShellWidensToFirstWord(t *testing.T) {
	rule := PermissionRuleFor(permCall("run_shell", `{"command":"npm test -- --watch"}`))
	if rule.Cmd != "npm *" || rule.Effect != PermissionAllow {
		t.Fatalf("unexpected shell rule: %+v", rule)
	}
	if rule := PermissionRuleFor(permCall("write_file", `{"path":"a.go"}`)); rule.Tool != "write_file" || rule.Cmd != "" || rule.Path != "" {
		t.Fatalf("unexpected file rule: %+v", rule)
	}
}

func TestPermissionRuleMalformedPatternFailsClosed(t *testing.T) {
	rules := []PermissionRule{{Tool: "[", Effect: PermissionAllow}}
	if _, ok := EvaluatePermission(rules, permCall("write_file", `{"path":"a.go"}`)); ok {
		t.Fatal("a malformed glob must match nothing rather than everything")
	}
}

// The mode gate is the boundary for terminal sessions — not the safeshell
// allowlist, which can only vet the opening command. Untagged on purpose, so
// the contract is checked on platforms where the pty itself does not exist.
func TestTerminalToolsAbsentFromReadOnlyModes(t *testing.T) {
	for _, name := range []string{"terminal_open", "terminal_send", "terminal_read", "terminal_list", "terminal_close"} {
		p := PolicyForName(name)
		if p.Allows(ModeExplore) || p.Allows(ModePlan) {
			t.Errorf("%s must be unreachable from explore and plan mode: %+v", name, p)
		}
		if !p.Allows(ModeWrite) || !p.Allows(ModeAuto) {
			t.Errorf("%s should be available in write and auto mode: %+v", name, p)
		}
		if !p.Destructive {
			t.Errorf("%s must be destructive so write mode prompts on every call: %+v", name, p)
		}
		if p.SmallModelSafe {
			t.Errorf("%s should not be offered to small models: %+v", name, p)
		}
		if p.Timeout <= 0 {
			t.Errorf("%s has no timeout", name)
		}
	}
	// A ceiling below the handler's own max wait would let the harness deadline
	// kill terminal_send before it can report its result.
	if got := PolicyForName("terminal_send").Timeout; got <= terminalMaxWait {
		t.Fatalf("terminal_send's deadline %s must outlive its own max wait %s", got, terminalMaxWait)
	}
}
