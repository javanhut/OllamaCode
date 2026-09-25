package tui

import "strings"

// Sampling defaults. The model's vendor knows which settings it was tuned for,
// and several warn against greedy decoding outright: Qwen3's model card says
// temperature 0 on a thinking model causes "performance degradation and endless
// repetitions". So the order is: the user's profile, then the Modelfile (library
// models carry the vendor's values there), then the vendor preset below for
// models whose Modelfile sets nothing, and only then ocode's small-model
// fallback. Ollama ignores repetition penalties, so none are listed.

// samplingPreset is one vendor's recommended settings. Zero fields are unset.
type samplingPreset struct {
	name        string
	temperature float64
	topP        float64
	topK        int
	minP        *float64
}

var minPOff = 0.0 // min_p explicitly disabled, as Qwen recommends

// Sources: the model cards on Hugging Face (Qwen/Qwen3-8B, Qwen3.6-27B,
// Qwen3-Coder-30B-A3B-Instruct, mistralai/Devstral-Small-2507,
// zai-org/GLM-4.7-Flash), github.com/openai/gpt-oss, and the Gemma team's
// published settings.
var (
	presetQwenThinking = samplingPreset{name: "qwen thinking", temperature: 0.6, topP: 0.95, topK: 20, minP: &minPOff}
	presetQwenInstruct = samplingPreset{name: "qwen instruct", temperature: 0.7, topP: 0.8, topK: 20, minP: &minPOff}
	presetQwenCoder    = samplingPreset{name: "qwen coder", temperature: 0.7, topP: 0.8, topK: 20}
	presetGPTOSS       = samplingPreset{name: "gpt-oss", temperature: 1.0, topP: 1.0}
	presetDevstral     = samplingPreset{name: "devstral", temperature: 0.15}
	presetDeepSeekR1   = samplingPreset{name: "deepseek-r1", temperature: 0.6, topP: 0.95}
	presetGemma        = samplingPreset{name: "gemma", temperature: 1.0, topP: 0.95, topK: 64}
	presetGLM          = samplingPreset{name: "glm", temperature: 0.7, topP: 1.0}
	// presetThinking stands in for a thinking model of no listed family: the
	// common recommendation, and above all not greedy.
	presetThinking = samplingPreset{name: "generic thinking", temperature: 0.6, topP: 0.95, topK: 20}
)

// familySampling picks the vendor preset for a model name. ok is false for a
// family with no preset. deepseek is checked before qwen so the R1 distills
// ("deepseek-r1-distill-qwen") get the reasoning settings.
func familySampling(model string, thinking bool) (samplingPreset, bool) {
	name := strings.ToLower(model)
	switch {
	case strings.Contains(name, "gpt-oss"):
		return presetGPTOSS, true
	case strings.Contains(name, "devstral"):
		return presetDevstral, true
	case strings.Contains(name, "deepseek-r1"), strings.Contains(name, "deepseek") && thinking:
		return presetDeepSeekR1, true
	case strings.Contains(name, "qwen") && strings.Contains(name, "coder"):
		return presetQwenCoder, true
	case strings.Contains(name, "qwen") && thinking:
		return presetQwenThinking, true
	case strings.Contains(name, "qwen"):
		return presetQwenInstruct, true
	case strings.Contains(name, "gemma"):
		return presetGemma, true
	case strings.Contains(name, "glm"):
		return presetGLM, true
	}
	return samplingPreset{}, false
}

func (p samplingPreset) options() map[string]any {
	opts := map[string]any{"temperature": p.temperature}
	if p.topP > 0 {
		opts["top_p"] = p.topP
	}
	if p.topK > 0 {
		opts["top_k"] = p.topK
	}
	if p.minP != nil {
		opts["min_p"] = *p.minP
	}
	return opts
}

// samplingOptions returns the sampling fields for a request; see
// samplingSource for which rule chose them.
func (m *Model) samplingOptions(action bool) map[string]any {
	opts, _ := m.samplingSource(action)
	return opts
}

// samplingSource resolves the sampling fields for an action (tools offered) or
// prose turn, and names the rule that chose them for /stats. A nil map sends
// nothing and leaves the choice to Ollama.
func (m *Model) samplingSource(action bool) (map[string]any, string) {
	return resolveSampling(m.profile, m.modelName, action)
}

// resolveSampling is samplingSource for any profile, so headless runs share
// the TUI's rules.
func resolveSampling(p ModelProfile, model string, action bool) (map[string]any, string) {
	switch {
	case p.Temperature != nil:
		return map[string]any{"temperature": *p.Temperature}, "profile temperature"
	case action && p.ActionTemperature != nil:
		return map[string]any{"temperature": *p.ActionTemperature}, "profile action_temperature"
	case !action && p.ProseTemperature != nil:
		return map[string]any{"temperature": *p.ProseTemperature}, "profile prose_temperature"
	case p.ModelfileSampling != nil && *p.ModelfileSampling:
		return nil, "the model's Modelfile"
	}
	if preset, ok := familySampling(model, p.SupportsThinking); ok {
		return preset.options(), preset.name + " preset"
	}
	if p.SupportsThinking {
		return presetThinking.options(), presetThinking.name + " preset"
	}
	if p.smallModel() {
		// A small non-thinking model of unknown family keeps the old default:
		// greedy on tool turns for argument stability, a little freer in prose.
		if action {
			return map[string]any{"temperature": 0.0}, "small-model greedy"
		}
		return map[string]any{"temperature": 0.2}, "small-model default"
	}
	return nil, "Ollama defaults"
}
