package tui

import (
	"encoding/json"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// toolCallConstraintCache is the process-wide fallback-ladder state for
// first-pass constrained decoding. Grammar acceptance is a model+host property
// (the cache keys on both), so one shared cache lets every loop in the process
// — interactive turns, sub-agents, parallel-edit planners — start at the rung
// that already worked instead of re-probing.
var toolCallConstraintCache = agent.NewConstraintCache()

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
//
// Opt-in per model (profile constrained_tool_calls: true). It used to be on
// for every small model, until cmd/eval measured it: gemma4 (Q4_K_M) lost 54
// points of outcome pass rate under it (19 tasks x 3, paired 95% CI -70 to
// -37) — the model routes tool calls into the prose escape in its native
// syntax — while ornith showed no difference. Turn it on for a model only
// after `cmd/eval -constrain -compare` says it helps.
func (m *Model) toolCallFormat(actionTurn bool, defs []tools.Tool) (json.RawMessage, bool) {
	if !actionTurn || !m.profile.constrainToolCalls() || !agent.ConstrainedDecodingSupported(m.host) {
		return nil, false
	}
	return toolCallConstraintCache.Format(agent.ConstraintKey(m.host, m.modelName), defs)
}

// downgradeToolCallFormat records a host rejection (HTTP 400 from the
// schema->grammar conversion) and reports whether a weaker rung exists to
// retry with.
func (m *Model) downgradeToolCallFormat() bool {
	return toolCallConstraintCache.Downgrade(agent.ConstraintKey(m.host, m.modelName))
}

// disableToolCallFormat drops straight to the unconstrained rung. The host
// accepted the schema, so no rejection will ever step the ladder down, but the
// model looped trying to satisfy it — acceptance is not compatibility.
func (m *Model) disableToolCallFormat() {
	toolCallConstraintCache.Disable(agent.ConstraintKey(m.host, m.modelName))
}

// subagentConstraintOptions passes the small-tier constraint posture (and the
// shared rung cache) to a headless child loop, so a small model routed into a
// sub-agent gets the same first-pass decoding help as the interactive loop.
func (m *Model) subagentConstraintOptions() (bool, *agent.ConstraintCache) {
	if !m.profile.constrainToolCalls() {
		return false, nil
	}
	return true, toolCallConstraintCache
}

// constrainToolCalls: small-tier models that opted in (see toolCallFormat).
func (p ModelProfile) constrainToolCalls() bool {
	return p.ConstrainedToolCalls != nil && *p.ConstrainedToolCalls && p.smallModel()
}
