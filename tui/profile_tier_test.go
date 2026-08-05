package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func TestSmallModelTier(t *testing.T) {
	cases := []struct {
		params float64
		small  bool
	}{
		{0, false}, // unknown → treated big (current behavior)
		{3, true},
		{12.4, true},
		{14.9, true},
		{15, false},
		{36, false},
		{1000, false}, // "1t" cloud model
	}
	for _, c := range cases {
		if got := (ModelProfile{ParamsB: c.params}).smallModel(); got != c.small {
			t.Errorf("smallModel(%vB) = %v, want %v", c.params, got, c.small)
		}
	}
}

func TestLeanToolsetForSmallModels(t *testing.T) {
	m := &Model{mode: WriteMode, tools: tools.DefaultRegistry()}

	m.profile = ModelProfile{ParamsB: 36}
	full := len(m.toolsForMode())

	m.profile = ModelProfile{ParamsB: 7}
	lean := m.toolsForMode()

	if len(lean) >= full {
		t.Fatalf("lean toolset (%d) should be smaller than full (%d)", len(lean), full)
	}
	for _, tool := range lean {
		if !tool.Policy.SmallModelSafe {
			t.Errorf("unexpected tool %q in lean set", tool.Function.Name)
		}
	}
	// The core edit workflow must survive the trim.
	names := map[string]bool{}
	for _, tool := range lean {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"read_file", "edit_file", "write_file", "grep", "run_shell"} {
		if !names[want] {
			t.Errorf("lean set is missing core tool %q", want)
		}
	}
}

func TestSmallModelExploreIncludesWebTools(t *testing.T) {
	m := &Model{
		mode:    ExploreMode,
		tools:   tools.DefaultRegistry(),
		profile: ModelProfile{ParamsB: 7},
	}

	names := map[string]bool{}
	for _, tool := range m.toolsForMode() {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"web_fetch", "web_search", "web_search_api", "web_crawl"} {
		if !names[want] {
			t.Errorf("small-model explore set is missing web tool %q", want)
		}
	}
}

func TestRelevantToolSelectionHonorsProfileCap(t *testing.T) {
	m := &Model{
		mode:    WriteMode,
		tools:   tools.DefaultRegistry(),
		profile: ModelProfile{CapabilityTier: "small", MaxVisibleTools: 10},
		history: []api.Message{{Role: "user", Content: "Look up the latest web documentation and fix the file"}},
	}
	selected := m.toolsForMode()
	if len(selected) != 10 {
		t.Fatalf("expected capped toolset of 10, got %d", len(selected))
	}
	names := map[string]bool{}
	for _, tool := range selected {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"web_search", "web_fetch", "read_file", "edit_file"} {
		if !names[want] {
			t.Errorf("task-aware selection omitted %q: %v", want, names)
		}
	}
}

func TestActiveSystemPromptByTier(t *testing.T) {
	m := &Model{profile: ModelProfile{ParamsB: 7}}
	if !strings.HasPrefix(m.activeSystemPrompt(), compactSystemPrompt) {
		t.Fatal("small model should get the compact prompt")
	}
	m.profile.ParamsB = 70
	if !strings.HasPrefix(m.activeSystemPrompt(), systemPrompt) {
		t.Fatal("big model should get the full prompt")
	}
	if len(compactSystemPrompt) > 3000 {
		t.Fatalf("compact prompt has grown to %d bytes — it exists to be small", len(compactSystemPrompt))
	}
	if len(systemPrompt) > 5000 {
		t.Fatalf("strong-model prompt has grown to %d bytes; keep policy in code and dynamic context", len(systemPrompt))
	}
}

func TestSmallModelTemperatureDefault(t *testing.T) {
	m := &Model{profile: ModelProfile{ParamsB: 7}, contextLimit: 8192}
	if temp, ok := m.chatOptions(true)["temperature"]; !ok || temp != 0.0 {
		t.Fatalf("small-model action turns should default temperature to 0, got %v", temp)
	}
	if temp := m.chatOptions(false)["temperature"]; temp != 0.2 {
		t.Fatalf("small-model prose turns should default temperature to 0.2, got %v", temp)
	}
	// Explicit override wins.
	override := 0.9
	m.profile.Temperature = &override
	if temp := m.chatOptions(true)["temperature"]; temp != 0.9 {
		t.Fatalf("explicit temperature should win, got %v", temp)
	}
	// Big model: untouched.
	m.profile = ModelProfile{ParamsB: 70}
	if _, ok := m.chatOptions(true)["temperature"]; ok {
		t.Fatal("big model without override should not set temperature")
	}
}

func TestCapabilityTierOverridesParameterCount(t *testing.T) {
	if (ModelProfile{ParamsB: 70, CapabilityTier: "small"}).smallModel() != true {
		t.Fatal("explicit small tier should override parameter count")
	}
	if (ModelProfile{ParamsB: 7, CapabilityTier: "strong"}).smallModel() != false {
		t.Fatal("explicit strong tier should override parameter count")
	}
	off := false
	if (ModelProfile{ParamsB: 70, ParallelTools: &off}).parallelToolCalls() {
		t.Fatal("parallel-tool override was ignored")
	}
	if got := (ModelProfile{CapabilityTier: "small"}).maxParallelToolCalls(); got != 1 {
		t.Fatalf("small models should execute one tool at a time, got %d", got)
	}
	if got := (ModelProfile{CapabilityTier: "strong", MaxParallelTools: 6}).maxParallelToolCalls(); got != 6 {
		t.Fatalf("parallel-tool cap ignored, got %d", got)
	}
	if (ModelProfile{CapabilityTier: "strong", Delegation: &off}).canDelegate() {
		t.Fatal("delegation override was ignored")
	}
	if !(ModelProfile{CapabilityTier: "strong"}).reviewPass() {
		t.Fatal("strong tier should default to an adversarial review pass")
	}
	if (ModelProfile{CapabilityTier: "strong", ReviewPass: &off}).reviewPass() {
		t.Fatal("review-pass override was ignored")
	}
}

func TestProfileDiscoveryPreservesCapabilityOverrides(t *testing.T) {
	off := false
	temp := 0.1
	got := preserveProfileOverrides(
		ModelProfile{NumCtx: 32768, ParamsB: 7, SupportsTools: true},
		ModelProfile{NumCtx: 65536, CapabilityTier: "strong", MaxVisibleTools: 30, ProfileMaxSteps: 60, ParallelTools: &off, MaxParallelTools: 3, Delegation: &off, ActionTemperature: &temp},
	)
	if got.ParamsB != 7 || !got.SupportsTools {
		t.Fatalf("discovered capabilities were lost: %+v", got)
	}
	if got.NumCtx != 65536 || got.CapabilityTier != "strong" || got.MaxVisibleTools != 30 || got.ProfileMaxSteps != 60 || got.ParallelTools == nil || *got.ParallelTools || got.MaxParallelTools != 3 || got.Delegation == nil || *got.Delegation || got.ActionTemperature == nil {
		t.Fatalf("configured overrides were lost: %+v", got)
	}
}

func TestRAGLimitsByCapability(t *testing.T) {
	small := &Model{profile: ModelProfile{CapabilityTier: "small"}}
	if topK, tokens := small.ragLimits(); topK != smallRAGTopK || tokens != smallRAGTokens {
		t.Fatalf("unexpected small-model RAG limits: %d, %d", topK, tokens)
	}
	strong := &Model{profile: ModelProfile{CapabilityTier: "strong", RAGTopK: 12, RAGTokens: 9000}}
	if topK, tokens := strong.ragLimits(); topK != 12 || tokens != 9000 {
		t.Fatalf("profile RAG overrides ignored: %d, %d", topK, tokens)
	}
}

func TestFallbackContextIsLocalHardwareSafe(t *testing.T) {
	if defaultContextLimit > 32768 {
		t.Fatalf("fallback context %d allocates too much KV cache when model introspection fails", defaultContextLimit)
	}
	if defaultContextLimit <= generationReserve {
		t.Fatalf("fallback context %d leaves no useful prompt budget", defaultContextLimit)
	}
}
