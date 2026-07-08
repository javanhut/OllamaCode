package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

// RepairArgsViaFormat asks the model, with JSON-schema-constrained decoding, to
// emit a corrected arguments object for a tool whose call failed on an argument
// problem. It's self-contained (no conversation history needed) and degrades
// gracefully — returning ok=false — when the model or Ollama version doesn't
// honor `format`. Shared by the TUI loop and the headless sub-agent loop.
func RepairArgsViaFormat(ctx context.Context, host ChatClient, reg *mcp.Registry, model string, numCtx int, call mcp.ToolCall) (json.RawMessage, bool) {
	tool, ok := reg.Lookup(call.Function.Name)
	if !ok {
		return nil, false
	}
	prompt := fmt.Sprintf(
		"You tried to call the tool %q but its arguments were malformed. Output ONLY a corrected JSON arguments object that matches the tool's schema — no prose, no code fences. Malformed attempt:\n%s",
		call.Function.Name, string(call.Function.Arguments),
	)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options := map[string]any{"temperature": 0}
	if numCtx > 0 {
		options["num_ctx"] = numCtx
	}
	resp, err := host.ChatOnce(cctx, api.ChatRequest{
		Model: model,
		Messages: []api.Message{
			{Role: "system", Content: "You output only valid JSON. No explanations."},
			{Role: "user", Content: prompt},
		},
		Format:  tool.Function.JSONSchema(),
		Options: options,
	})
	if err != nil {
		return nil, false
	}
	out := mcp.SalvageJSON(json.RawMessage(strings.TrimSpace(resp.Message.Content)))
	if len(out) == 0 || !json.Valid(out) {
		return nil, false
	}
	return out, true
}
