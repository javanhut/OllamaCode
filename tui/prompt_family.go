package tui

import "strings"

// Per-family prompt sections. One static prompt for every model works poorly:
// qwen and llama small models slip into narrating between tool calls, and
// reasoner-style deepseek models re-reason aloud after their hidden thinking.
// Instead of duplicating the full prompt per family (opencode ships whole
// per-provider prompts), each family gets a short "behavior notes" section
// appended to the base prompt. The section is static per model, so it rides in
// the KV-cached prefix (see activeSystemPrompt) and never the volatile tail.

// detectPromptFamily maps a model name to a prompt family via lowercase
// substring matching — the same heuristic style as tier detection. deepseek is
// checked first so r1 distills ("deepseek-r1-distill-qwen-7b") get the
// reasoning notes rather than the plain qwen section. An empty result means
// "no family wording": the base prompt goes out unchanged.
func detectPromptFamily(model string) string {
	name := strings.ToLower(model)
	switch {
	case strings.Contains(name, "deepseek"):
		return "deepseek"
	case strings.Contains(name, "qwen"):
		return "qwen"
	case strings.Contains(name, "llama"):
		return "llama"
	case strings.Contains(name, "mistral"), strings.Contains(name, "mixtral"):
		return "mistral"
	case strings.Contains(name, "phi"):
		return "phi"
	case strings.Contains(name, "gemma"):
		return "gemma"
	case strings.Contains(name, "gpt"), strings.Contains(name, "openai"):
		return "gpt"
	default:
		return ""
	}
}

// promptFamily resolves the active family: the prompt_family config override
// wins when set ("none"/"default" = base prompt only), otherwise the family is
// auto-detected from the model name.
func (m *Model) promptFamily() string {
	override := strings.ToLower(strings.TrimSpace(m.cfg.PromptFamily))
	switch override {
	case "":
		return detectPromptFamily(m.modelName)
	case "none", "default":
		return ""
	default:
		return override
	}
}

// familyPromptSection returns the behavior-notes section for the active
// family, or "" when the base prompt applies unchanged. Sections exist only
// where a family's behavior genuinely differs from the base prompt's
// assumptions — an override naming a family with no section is honored as
// "no section", not an error.
func (m *Model) familyPromptSection() string {
	switch m.promptFamily() {
	case "qwen", "llama":
		// Tool-call discipline is a small-model failure mode; big qwen/llama
		// follow the base prompt's batching rule fine on their own.
		if m.profile.smallModel() {
			return smallToolDisciplineSection
		}
	case "deepseek":
		return deepseekReasoningSection
	}
	return ""
}

// smallToolDisciplineSection reinforces the rules small qwen and llama models
// break most: narrating between calls, wrapping arguments in fences or prose,
// and guessing argument values. It deliberately restates compactSystemPrompt's
// TOOL RULES in sharper wording rather than adding new ones — a contradiction
// here would cost more than the emphasis saves.
const smallToolDisciplineSection = `

TOOL-CALL DISCIPLINE:
- Emit the tool call(s) for this turn, then STOP. No commentary before, between, or after tool calls — the user sees the results directly.
- Arguments are one JSON object with exactly the declared fields: no markdown fences, no comments, no placeholders like "..." or "<path>".
- Never guess an argument value you can look up. Read the file or grep first.
- If a call fails, fix the arguments or change approach; do not repeat it unchanged.`

// deepseekReasoningSection addresses reasoner-style deepseek models (r1 and
// its distills): the thinking stream is hidden from the user and stripped
// before replies are re-sent (see deriveModelMessages), so conclusions must
// surface in visible content, and post-reasoning output should act instead of
// re-reasoning aloud.
const deepseekReasoningSection = `

REASONING MODEL NOTES:
- Your reasoning is hidden from the user — they see only your final text and tool calls. State conclusions and decisions in the visible reply; never assume something you only thought was communicated.
- Once you have finished reasoning, ACT: emit the tool call or final answer directly. Do not recap your reasoning as prose first.
- Keep visible text between tool calls to one short line of intent at most.`
