package tui

import (
	"context"
	"encoding/json"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// repairArgsViaFormat asks the model, with JSON-schema-constrained decoding, to
// emit a corrected arguments object for the given tool. It delegates to the
// shared agent implementation so the TUI loop and the headless sub-agent loop
// share one code path. Degrades gracefully (ok=false) when the model or Ollama
// version doesn't honor `format`.
func (m *Model) repairArgsViaFormat(call tools.ToolCall) (json.RawMessage, bool) {
	return agent.RepairArgsViaFormat(context.Background(), m.host, m.tools, m.modelName, m.contextLimit, call)
}
