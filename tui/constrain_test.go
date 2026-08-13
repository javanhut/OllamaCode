package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func testConstraintDefs(t *testing.T) []tools.Tool {
	t.Helper()
	r := tools.NewRegistry()
	r.Register(tools.Tool{Function: tools.Function{
		Name: "gate_echo",
		Parameters: tools.Schema{Type: "object",
			Properties: map[string]tools.Property{"text": {Type: "string"}},
			Required:   []string{"text"}},
	}})
	return r.Definitions()
}

// The interactive loop's constraint gate must mirror the small-model posture:
// constrained on small-tier action turns against native Ollama, untouched
// everywhere else. Model names are unique per subtest because the rung cache
// is process-wide and keyed by host+model.
func TestToolCallFormatGating(t *testing.T) {
	defs := testConstraintDefs(t)

	native := api.OllamaHost{}
	native.SetURI("http://localhost:11434")

	t.Run("small tier action turn is constrained", func(t *testing.T) {
		m := &Model{profile: ModelProfile{ParamsB: 7}, modelName: "gate-small", host: native}
		raw, ok := m.toolCallFormat(true, defs)
		if !ok {
			t.Fatal("expected a format for a small-tier action turn")
		}
		s := string(raw)
		if !strings.Contains(s, "anyOf") || !strings.Contains(s, `"gate_echo"`) || !strings.Contains(s, `"response"`) {
			t.Fatalf("expected full union with the turn's tools and a prose escape, got %s", s)
		}
	})

	t.Run("prose turn is never constrained", func(t *testing.T) {
		m := &Model{profile: ModelProfile{ParamsB: 7}, modelName: "gate-prose", host: native}
		if raw, ok := m.toolCallFormat(false, defs); ok {
			t.Fatalf("tool-less turns must stay unconstrained, got %s", raw)
		}
	})

	t.Run("capable and strong tiers are untouched", func(t *testing.T) {
		for _, p := range []ModelProfile{{ParamsB: 70}, {ParamsB: 7, CapabilityTier: "strong"}} {
			m := &Model{profile: p, modelName: "gate-strong", host: native}
			if raw, ok := m.toolCallFormat(true, defs); ok {
				t.Fatalf("profile %+v must stay on native tool calls, got %s", p, raw)
			}
		}
	})

	t.Run("openai provider keeps the repair-only path", func(t *testing.T) {
		host := api.OllamaHost{}
		host.SetProvider(api.ProviderOpenAI)
		m := &Model{profile: ModelProfile{ParamsB: 7}, modelName: "gate-openai", host: host}
		if raw, ok := m.toolCallFormat(true, defs); ok {
			t.Fatalf("OpenAI-compatible providers must not get the Ollama grammar schema, got %s", raw)
		}
	})

	t.Run("ornith uses native tool calls", func(t *testing.T) {
		m := &Model{profile: ModelProfile{ParamsB: 9}, modelName: "ornith:latest", host: native}
		if raw, ok := m.toolCallFormat(true, defs); ok {
			t.Fatalf("Ornith must avoid the schema path that causes output loops, got %s", raw)
		}
		constrain, cache := m.subagentConstraintOptions()
		if constrain || cache != nil {
			t.Fatal("Ornith sub-agents must also use native tool calls")
		}
	})

	t.Run("rejection steps the cached rung down", func(t *testing.T) {
		m := &Model{profile: ModelProfile{ParamsB: 7}, modelName: "gate-ladder", host: native}
		first, ok := m.toolCallFormat(true, defs)
		if !ok || !strings.Contains(string(first), "anyOf") {
			t.Fatalf("first probe should be the union, got ok=%v %s", ok, first)
		}
		if !m.downgradeToolCallFormat() {
			t.Fatal("downgrade should offer a weaker rung")
		}
		next, ok := m.toolCallFormat(true, defs)
		if !ok || strings.Contains(string(next), "anyOf") || !strings.Contains(string(next), `"enum"`) {
			t.Fatalf("after rejection the flat enum rung should be served, got ok=%v %s", ok, next)
		}
	})

	t.Run("subagent options inherit the small-tier posture", func(t *testing.T) {
		small := &Model{profile: ModelProfile{ParamsB: 7}}
		constrain, cache := small.subagentConstraintOptions()
		if !constrain || cache == nil {
			t.Fatal("small-tier sub-agents should share the constraint posture and cache")
		}
		strong := &Model{profile: ModelProfile{ParamsB: 70}}
		constrain, cache = strong.subagentConstraintOptions()
		if constrain || cache != nil {
			t.Fatal("strong-tier sub-agents stay on native tool calls")
		}
	})
}
