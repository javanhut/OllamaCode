package tui

import (
	"strings"
	"testing"
)

func TestDetectPromptFamily(t *testing.T) {
	cases := []struct {
		model  string
		family string
	}{
		{"qwen2.5-coder:7b", "qwen"},
		{"Qwen3-32B", "qwen"},
		{"llama3.1:8b", "llama"},
		{"codellama:13b", "llama"},
		{"mistral:7b", "mistral"},
		{"mixtral:8x7b", "mistral"},
		{"deepseek-coder-v2:16b", "deepseek"},
		{"deepseek-r1:8b", "deepseek"},
		{"deepseek-r1-distill-qwen-7b", "deepseek"}, // distill: reasoning notes beat the plain qwen section
		{"phi4:14b", "phi"},
		{"gemma3:12b", "gemma"},
		{"gpt-4o", "gpt"},
		{"gpt-oss:20b", "gpt"},
		{"openai/gpt-5", "gpt"},
		{"granite-code:8b", ""},
		{"starcoder2:7b", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := detectPromptFamily(c.model); got != c.family {
			t.Errorf("detectPromptFamily(%q) = %q, want %q", c.model, got, c.family)
		}
	}
}

func TestFamilySectionSelection(t *testing.T) {
	// Small qwen: tool-call discipline appended to the compact base.
	m := &Model{modelName: "qwen2.5-coder:7b", profile: ModelProfile{ParamsB: 7}}
	got := m.activeSystemPrompt()
	if !strings.HasPrefix(got, compactSystemPrompt) {
		t.Fatal("small qwen should keep the compact base prompt")
	}
	if !strings.Contains(got, smallToolDisciplineSection) {
		t.Fatal("small qwen should get the tool-discipline section")
	}

	// Small llama gets the same section.
	m = &Model{modelName: "llama3.1:8b", profile: ModelProfile{ParamsB: 8}}
	if !strings.Contains(m.activeSystemPrompt(), smallToolDisciplineSection) {
		t.Fatal("small llama should get the tool-discipline section")
	}

	// Big qwen follows the base prompt's batching rule fine — no section.
	m = &Model{modelName: "qwen2.5-coder:32b", profile: ModelProfile{ParamsB: 32}}
	if strings.Contains(m.activeSystemPrompt(), smallToolDisciplineSection) {
		t.Fatal("big qwen should not get the small-model discipline section")
	}

	// Deepseek gets the reasoning notes at any size, on either base prompt.
	m = &Model{modelName: "deepseek-r1:70b", profile: ModelProfile{ParamsB: 70}}
	got = m.activeSystemPrompt()
	if !strings.HasPrefix(got, systemPrompt) || !strings.Contains(got, deepseekReasoningSection) {
		t.Fatal("big deepseek should get the full prompt plus the reasoning section")
	}
	m = &Model{modelName: "deepseek-r1:8b", profile: ModelProfile{ParamsB: 8}}
	got = m.activeSystemPrompt()
	if !strings.HasPrefix(got, compactSystemPrompt) || !strings.Contains(got, deepseekReasoningSection) {
		t.Fatal("small deepseek should get the compact prompt plus the reasoning section")
	}

	// Unknown family: base prompt + environment block, nothing else.
	m = &Model{modelName: "granite-code:8b", profile: ModelProfile{ParamsB: 8}}
	if got := m.activeSystemPrompt(); got != compactSystemPrompt+environmentBlock() {
		t.Fatal("unknown family should get the bare base prompt")
	}
}

func TestPromptFamilyConfigOverride(t *testing.T) {
	// "none" disables auto-detection.
	m := &Model{modelName: "qwen2.5-coder:7b", profile: ModelProfile{ParamsB: 7}, cfg: config{PromptFamily: "none"}}
	if strings.Contains(m.activeSystemPrompt(), smallToolDisciplineSection) {
		t.Fatal(`prompt_family "none" should suppress the family section`)
	}
	// "default" means the same.
	m.cfg.PromptFamily = "default"
	if strings.Contains(m.activeSystemPrompt(), smallToolDisciplineSection) {
		t.Fatal(`prompt_family "default" should suppress the family section`)
	}
	// Forcing a family applies its section even when detection wouldn't.
	m = &Model{modelName: "granite-code:8b", profile: ModelProfile{ParamsB: 8}, cfg: config{PromptFamily: "deepseek"}}
	if !strings.Contains(m.activeSystemPrompt(), deepseekReasoningSection) {
		t.Fatal("prompt_family override should force the named family's section")
	}
	// Forcing a family with no section for this tier yields the bare base.
	m = &Model{modelName: "granite-code:34b", profile: ModelProfile{ParamsB: 34}, cfg: config{PromptFamily: "qwen"}}
	if got := m.activeSystemPrompt(); got != systemPrompt+environmentBlock() {
		t.Fatal("override naming a family with no applicable section should give the bare base prompt")
	}
	// Empty override = auto-detect.
	m = &Model{modelName: "qwen2.5-coder:7b", profile: ModelProfile{ParamsB: 7}, cfg: config{PromptFamily: " "}}
	if !strings.Contains(m.activeSystemPrompt(), smallToolDisciplineSection) {
		t.Fatal("empty prompt_family should auto-detect")
	}
}

// The family section is static per model, so it must live in the cached system
// prefix (first message) and never leak into the volatile dynamic tail (last
// message) — see assembleMessages' ordering contract.
func TestFamilySectionStaysInStaticPrefix(t *testing.T) {
	m := &Model{
		modelName:    "qwen2.5-coder:7b",
		profile:      ModelProfile{ParamsB: 7},
		notes:        &sessionNotes{},
		contextLimit: 32768,
	}
	msgs := m.assembleMessages("")
	if len(msgs) < 2 {
		t.Fatalf("expected at least static prompt and dynamic tail, got %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, smallToolDisciplineSection) {
		t.Fatal("family section missing from the static system prompt")
	}
	tail := msgs[len(msgs)-1].Content
	if strings.Contains(tail, smallToolDisciplineSection) || strings.Contains(tail, "TOOL-CALL DISCIPLINE") {
		t.Fatal("family section leaked into the volatile dynamic tail")
	}
}
