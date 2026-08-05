package tui

import (
	"strings"
	"testing"

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
		if !leanToolNames[tool.Function.Name] {
			t.Errorf("unexpected tool %q in lean set", tool.Function.Name)
		}
	}
	// The core edit workflow must survive the trim.
	names := map[string]bool{}
	for _, tool := range lean {
		names[tool.Function.Name] = true
	}
	// (switch_mode/todo_write are registered by the TUI at startup, not in
	// DefaultRegistry, so they're asserted via leanToolNames membership only.)
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
}

func TestSmallModelTemperatureDefault(t *testing.T) {
	m := &Model{profile: ModelProfile{ParamsB: 7}, contextLimit: 8192}
	if temp, ok := m.chatOptions()["temperature"]; !ok || temp != 0.2 {
		t.Fatalf("small model should default temperature to 0.2, got %v", temp)
	}
	// Explicit override wins.
	override := 0.9
	m.profile.Temperature = &override
	if temp := m.chatOptions()["temperature"]; temp != 0.9 {
		t.Fatalf("explicit temperature should win, got %v", temp)
	}
	// Big model: untouched.
	m.profile = ModelProfile{ParamsB: 70}
	if _, ok := m.chatOptions()["temperature"]; ok {
		t.Fatal("big model without override should not set temperature")
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
