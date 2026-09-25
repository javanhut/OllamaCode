package tui

import (
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestResolveSampling(t *testing.T) {
	small := ModelProfile{ParamsB: 8}
	cases := []struct {
		name    string
		profile ModelProfile
		model   string
		action  bool
		want    map[string]any
		source  string
	}{
		{"qwen thinking is never greedy", ModelProfile{ParamsB: 8, SupportsThinking: true}, "qwen3:8b", true,
			map[string]any{"temperature": 0.6, "top_p": 0.95, "top_k": 20, "min_p": 0.0}, "qwen thinking preset"},
		{"qwen instruct", small, "qwen3:4b-instruct-2507", true,
			map[string]any{"temperature": 0.7, "top_p": 0.8, "top_k": 20, "min_p": 0.0}, "qwen instruct preset"},
		{"qwen coder", ModelProfile{ParamsB: 30}, "qwen3-coder:30b", true,
			map[string]any{"temperature": 0.7, "top_p": 0.8, "top_k": 20}, "qwen coder preset"},
		{"r1 distill is deepseek, not qwen", ModelProfile{SupportsThinking: true}, "deepseek-r1-distill-qwen-7b", true,
			map[string]any{"temperature": 0.6, "top_p": 0.95}, "deepseek-r1 preset"},
		{"gpt-oss", ModelProfile{SupportsThinking: true}, "gpt-oss:20b", true,
			map[string]any{"temperature": 1.0, "top_p": 1.0}, "gpt-oss preset"},
		{"modelfile wins over the preset", ModelProfile{SupportsThinking: true, ModelfileSampling: ptr(true)}, "qwen3:8b", true,
			nil, "the model's Modelfile"},
		{"profile wins over the modelfile", ModelProfile{Temperature: ptr(0.3), ModelfileSampling: ptr(true)}, "qwen3:8b", true,
			map[string]any{"temperature": 0.3}, "profile temperature"},
		{"action_temperature restores greedy", ModelProfile{ActionTemperature: ptr(0.0), ModelfileSampling: ptr(true)}, "qwen3:8b", true,
			map[string]any{"temperature": 0.0}, "profile action_temperature"},
		{"unknown small thinking model", ModelProfile{ParamsB: 7, SupportsThinking: true}, "mystery:7b", true,
			map[string]any{"temperature": 0.6, "top_p": 0.95, "top_k": 20}, "generic thinking preset"},
		{"unknown small model keeps greedy tool turns", small, "mystery:7b", true,
			map[string]any{"temperature": 0.0}, "small-model greedy"},
		{"unknown small model prose", small, "mystery:7b", false,
			map[string]any{"temperature": 0.2}, "small-model default"},
		{"unknown large model sends nothing", ModelProfile{ParamsB: 70}, "mystery:70b", true,
			nil, "Ollama defaults"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, source := resolveSampling(c.profile, c.model, c.action)
			if !reflect.DeepEqual(got, c.want) || source != c.source {
				t.Fatalf("got %v (%s), want %v (%s)", got, source, c.want, c.source)
			}
		})
	}
}

func TestChatOptionsKeepsNumCtxAndTopPOverride(t *testing.T) {
	m := &Model{modelName: "qwen3:8b", contextLimit: 65536,
		profile: ModelProfile{SupportsThinking: true, TopP: ptr(0.9)}}
	opts := m.chatOptions(true)
	if opts["num_ctx"] != 65536 || opts["temperature"] != 0.6 || opts["top_p"] != 0.9 || opts["top_k"] != 20 {
		t.Fatalf("options = %v", opts)
	}
}
