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
