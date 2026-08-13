package tui

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// toolCallConstraintCache is the process-wide fallback-ladder state for
// first-pass constrained decoding. Grammar acceptance is a model+host property
// (the cache keys on both), so one shared cache lets every loop in the process
// — interactive turns, sub-agents, parallel-edit planners — start at the rung
// that already worked instead of re-probing.
var toolCallConstraintCache = agent.NewConstraintCache()

var errRunawayModelStream = errors.New("model stream entered a repetitive output loop")

// toolCallFormat returns the constrained-decoding format for this request, or
// ok=false when the turn must stay unconstrained. The gates mirror the
// small-model action-turn posture already used for temperature and batching:
//   - actionTurn (tools attached): prose turns — loop-break and budget
//     finalizations send no tools — are never constrained;
//   - small tier: capable/strong models keep their native tool-call path;
//   - native Ollama only: OpenAI-compatible providers keep the repair path.
//
// The schema always includes a {"response": ...} escape branch, so a
// constrained model can still answer in prose when no tool is needed.
func (m *Model) toolCallFormat(actionTurn bool, defs []tools.Tool) (json.RawMessage, bool) {
	if !actionTurn || !m.profile.smallModel() || !m.constraintCompatibleModel() || !agent.ConstrainedDecodingSupported(m.host) {
		return nil, false
	}
	return toolCallConstraintCache.Format(agent.ConstraintKey(m.host, m.modelName), defs)
}

// Ornith currently accepts Ollama's JSON schema but can get trapped emitting
// concatenated prose and switch_mode objects forever. Its native tool-call path
// plus content repair is safer than schema-constrained decoding.
func (m *Model) constraintCompatibleModel() bool {
	name := strings.ToLower(strings.TrimSpace(m.modelName))
	return name != "ornith" && !strings.HasPrefix(name, "ornith:")
}

// downgradeToolCallFormat records a host rejection (HTTP 400 from the
// schema->grammar conversion) and reports whether a weaker rung exists to
// retry with.
func (m *Model) downgradeToolCallFormat() bool {
	return toolCallConstraintCache.Downgrade(agent.ConstraintKey(m.host, m.modelName))
}

func (m *Model) disableToolCallFormat() {
	toolCallConstraintCache.Disable(agent.ConstraintKey(m.host, m.modelName))
}

// subagentConstraintOptions passes the small-tier constraint posture (and the
// shared rung cache) to a headless child loop, so a small model routed into a
// sub-agent gets the same first-pass decoding help as the interactive loop.
func (m *Model) subagentConstraintOptions() (bool, *agent.ConstraintCache) {
	if !m.profile.smallModel() || !m.constraintCompatibleModel() {
		return false, nil
	}
	return true, toolCallConstraintCache
}
