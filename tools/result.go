package tools

import (
	"encoding/json"
	"strings"
)

const defaultResultLimit = 12 * 1024

// ResultEnvelope is the provider-independent result passed back to a model.
// Keeping success, evidence, and recovery guidance in stable fields makes tool
// output easier for small models to interpret and gives traces/evals a common
// contract without changing individual handlers.
type ResultEnvelope struct {
	OK        bool            `json:"ok"`
	Summary   string          `json:"summary"`
	Evidence  []string        `json:"evidence,omitempty"`
	Retryable bool            `json:"retryable,omitempty"`
	Hint      string          `json:"hint,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

func EncodeToolSuccess(toolName, output string) string {
	output, truncated := truncateResult(output, defaultResultLimit)
	env := ResultEnvelope{OK: true, Summary: toolName + " completed", Truncated: truncated}
	trimmed := strings.TrimSpace(output)
	if json.Valid([]byte(trimmed)) {
		env.Data = json.RawMessage(trimmed)
	} else if trimmed != "" {
		env.Evidence = []string{trimmed}
	}
	return marshalEnvelope(env)
}

func EncodeToolFailure(summary, hint string, retryable bool) string {
	hint, truncated := truncateResult(strings.TrimSpace(hint), defaultResultLimit)
	return marshalEnvelope(ResultEnvelope{
		OK: false, Summary: strings.TrimSpace(summary), Retryable: retryable,
		Hint: hint, Truncated: truncated,
	})
}

func DecodeToolResult(raw string) (ResultEnvelope, bool) {
	var result ResultEnvelope
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Summary == "" {
		return ResultEnvelope{}, false
	}
	return result, true
}

// ToolResultOK understands both the structured contract and legacy handler
// output so callers can migrate without misclassifying failures.
func ToolResultOK(raw string) bool {
	if result, ok := DecodeToolResult(raw); ok {
		return result.OK
	}
	return !strings.HasPrefix(strings.TrimSpace(raw), "error:")
}

func marshalEnvelope(result ResultEnvelope) string {
	b, err := json.Marshal(result)
	if err != nil {
		return `{"ok":false,"summary":"failed to encode tool result"}`
	}
	return string(b)
}

func truncateResult(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	const marker = "\n…[tool output truncated; final evidence retained]…\n"
	keep := limit - len(marker)
	if keep < 2 {
		return value[:limit], true
	}
	head := keep / 3
	tail := keep - head
	return value[:head] + marker + value[len(value)-tail:], true
}
