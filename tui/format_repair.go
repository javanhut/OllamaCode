package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

// shouldFormatRepair reports whether a failed tool call looks like an ARGUMENT
// problem (bad schema or broken JSON) worth escalating to constrained decoding,
// as opposed to a legitimate execution error (e.g. "file not found") that
// re-emitting arguments wouldn't fix.
func (m *Model) shouldFormatRepair(call mcp.ToolCall, err error) bool {
	var ve *mcp.ValidationError
	if errors.As(err, &ve) {
		return true
	}
	return len(call.Function.Arguments) > 0 && !json.Valid(call.Function.Arguments)
}

// repairArgsViaFormat asks the model, with JSON-schema-constrained decoding, to
// emit a corrected arguments object for the given tool. It's self-contained (no
// conversation history needed) so it's safe to call from a tool goroutine, and
// degrades gracefully — returning ok=false — when the model or Ollama version
// doesn't honor `format`.
func (m *Model) repairArgsViaFormat(call mcp.ToolCall) (json.RawMessage, bool) {
	tool, ok := m.tools.Lookup(call.Function.Name)
	if !ok {
		return nil, false
	}
	prompt := fmt.Sprintf(
		"You tried to call the tool %q but its arguments were malformed. Output ONLY a corrected JSON arguments object that matches the tool's schema — no prose, no code fences. Malformed attempt:\n%s",
		call.Function.Name, string(call.Function.Arguments),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := m.host.ChatOnce(ctx, api.ChatRequest{
		Model: m.modelName,
		Messages: []api.Message{
			{Role: "system", Content: "You output only valid JSON. No explanations."},
			{Role: "user", Content: prompt},
		},
		Format:  tool.Function.JSONSchema(),
		Options: map[string]any{"num_ctx": m.contextLimit, "temperature": 0},
	})
	if err != nil {
		return nil, false
	}
	out := salvageJSON(json.RawMessage(strings.TrimSpace(resp.Message.Content)))
	if len(out) == 0 || !json.Valid(out) {
		return nil, false
	}
	return out, true
}
