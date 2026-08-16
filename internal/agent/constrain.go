package agent

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// First-pass constrained decoding (Ollama's `format` parameter) for small-tier
// action turns.
//
// Small models routinely emit tool calls as prose-flavoured JSON content,
// invent tool names, or wrap the call in commentary and code fences. Constraining
// the response to a schema that IS one tool call removes those failure classes
// at decode time instead of repairing them after the fact (repair.go stays as
// the fallback for argument-level mistakes on an otherwise valid call).
//
// Every schema carries a {"response": "..."} escape branch so the model can
// still answer in prose when no tool is needed — without it a constrained
// model could never finish a turn or refuse a tool it doesn't have. The
// tool-call shape matches what ParseToolCallsFromContent already recognizes
// ({"name": ..., "arguments": {...}}), so a host that leaves the JSON in
// content (older Ollama) lands in the same rescue path as an unconstrained
// model, while a current Ollama elevates it to a native tool call.

// FormatRung identifies one level of the constrained-decoding fallback ladder.
// A host that rejects a rung (HTTP 400 from the schema->grammar conversion) is
// retried at the next-weaker rung, and the working rung is cached per
// model+host so the probe cost is paid once per process, not once per turn.
type FormatRung int

const (
	// RungFullUnion constrains to anyOf[per-tool {name, arguments} shapes,
	// prose envelope]: the strongest form, tying each tool's name to its own
	// argument schema at decode time.
	RungFullUnion FormatRung = iota
	// RungNameEnum is a single flat object — no anyOf — with the tool names
	// as one enum, a generic arguments object, and the prose escape. It
	// survives hosts whose grammar conversion chokes on unions while still
	// eliminating invented tool names and non-JSON output.
	RungNameEnum
	// RungPlainJSON is format:"json" — only "the reply must be JSON" remains.
	RungPlainJSON
	// RungOff disables constrained decoding (terminal rung: status quo).
	RungOff
)

func (r FormatRung) String() string {
	switch r {
	case RungFullUnion:
		return "full-union"
	case RungNameEnum:
		return "name-enum"
	case RungPlainJSON:
		return "json"
	default:
		return "off"
	}
}

// proseFieldDescription documents the escape branch inside the schema itself,
// so the model sees when NOT to call a tool.
const proseFieldDescription = "Final prose answer to the user. Use this ONLY when no tool call is needed."

// ConstrainedDecodingSupported reports whether first-pass constrained decoding
// should be attempted against this host. Only native Ollama's schema->grammar
// path is exercised: OpenAI-compatible endpoints translate Format into their
// own response_format with provider-specific restrictions (strict schemas, no
// anyOf on several of them), so they keep the repair-only path.
func ConstrainedDecodingSupported(host ChatClient) bool {
	kind, ok := host.(interface {
		IsOpenAI() bool
		IsCursor() bool
	})
	if !ok || kind.IsOpenAI() || kind.IsCursor() {
		return false
	}
	if caps, ok := host.(interface {
		ProviderCapabilities() api.ProviderCapabilities
	}); ok {
		return caps.ProviderCapabilities().StructuredOutput
	}
	return true
}

// ConstraintKey identifies a model+host pair for the rung cache. Grammar
// acceptance is a host/runtime property, but keying the model too keeps a
// routed session (same host, several models) from sharing a probe result with
// a model whose template handles constraint poorly.
func ConstraintKey(host ChatClient, model string) string {
	base := ""
	if u, ok := host.(interface{ URL() string }); ok {
		base = u.URL()
	}
	return base + "\x00" + model
}

// ToolCallFormat builds the format payload for a rung from the exact tool list
// attached to the request, so the enum can never name a tool the turn doesn't
// offer.
func ToolCallFormat(rung FormatRung, defs []tools.Tool) json.RawMessage {
	switch rung {
	case RungFullUnion:
		branches := make([]any, 0, len(defs)+1)
		for _, def := range defs {
			branches = append(branches, map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"name", "arguments"},
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "enum": []string{def.Function.Name}},
					"arguments": def.Function.JSONSchema(),
				},
			})
		}
		branches = append(branches, proseBranch())
		return mustMarshal(map[string]any{"anyOf": branches})
	case RungNameEnum:
		names := make([]string, 0, len(defs))
		for _, def := range defs {
			names = append(names, def.Function.Name)
		}
		return mustMarshal(map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "enum": names},
				"arguments": map[string]any{"type": "object"},
				"response":  map[string]any{"type": "string", "description": proseFieldDescription},
			},
		})
	case RungPlainJSON:
		return json.RawMessage(`"json"`)
	}
	return nil
}

func proseBranch() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"response"},
		"properties": map[string]any{
			"response": map[string]any{"type": "string", "description": proseFieldDescription},
		},
	}
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ConstraintCache remembers the strongest rung a model+host pair has accepted.
// Safe for concurrent use: parallel sub-agents share the parent's cache.
type ConstraintCache struct {
	mu    sync.Mutex
	rungs map[string]FormatRung
}

func NewConstraintCache() *ConstraintCache {
	return &ConstraintCache{rungs: map[string]FormatRung{}}
}

// Format returns the format payload for the pair's current rung, or ok=false
// when constrained decoding is off for it (rejected down to RungOff earlier)
// or there is nothing to constrain to.
func (c *ConstraintCache) Format(key string, defs []tools.Tool) (json.RawMessage, bool) {
	if len(defs) == 0 {
		return nil, false
	}
	c.mu.Lock()
	rung, ok := c.rungs[key]
	c.mu.Unlock()
	if !ok {
		rung = RungFullUnion // optimistic: probe the strongest schema first
	}
	if rung == RungOff {
		return nil, false
	}
	return ToolCallFormat(rung, defs), true
}

// Downgrade records a host rejection of the current rung and steps the cache
// down the ladder. Returns false only at RungOff, where no weaker request
// exists and the caller should surface the error normally.
func (c *ConstraintCache) Downgrade(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rung, ok := c.rungs[key]
	if !ok {
		rung = RungFullUnion
	}
	if rung >= RungOff {
		return false
	}
	c.rungs[key] = rung + 1
	return true
}

// IsFormatRejection reports whether an /api/chat error is the host refusing
// the format payload — Ollama answers schema->grammar conversion failures with
// a 400 whose body names the schema — as opposed to a transient transport
// failure, which must not move the rung cache.
//
// A context overflow is excluded here rather than at the call sites: this reads
// ANY 400, and a host answering an oversized prompt with one is not objecting
// to the schema. Treating it as a rejection steps the rung cache down
// permanently for that model+host over a format nobody complained about, and in
// the sub-agent ladder (agent.go) burns every rung on a request that overflows
// identically at all of them.
func IsFormatRejection(err error) bool {
	if err == nil || IsContextOverflow(err) {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "status code: 400") || strings.Contains(s, "schema conversion failed")
}

// contextOverflowMarkers are the phrases providers put in the BODY of an
// oversized-prompt refusal (llama.cpp, Ollama, OpenAI, Anthropic). The status
// code can't classify it — the same 400 also carries schema rejections — so the
// match is on the body, which api.statusError and the OpenAI adapter both embed
// in the error string.
var contextOverflowMarkers = []string{
	"context_length_exceeded",
	"context length exceeded",
	"maximum context length",
	"exceeds the available context size",
	"exceeds context length",
	"input length exceeds",
	"exceeds the context window",
	"prompt is too long",
}

// IsContextOverflow reports whether err is the provider refusing a prompt that
// does not fit its context window. Unlike a transport failure this is not
// transient — the identical request fails identically every time — so the only
// useful response is to shrink the request, never to retry it unchanged.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// UnwrapConstrainedProse recovers a prose answer from the constrained envelope:
// the model chose the {"response": "..."} branch (or, on the plain-JSON rung,
// replied with a bare JSON string). Call it only for responses to requests
// that carried a ToolCallFormat payload — unconditional unwrapping could
// hijack an unconstrained model's legitimate JSON-shaped prose.
func UnwrapConstrainedProse(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", false
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal([]byte(trimmed), &s) == nil && strings.TrimSpace(s) != "" {
			return s, true
		}
		return "", false
	}
	if trimmed[0] != '{' {
		return "", false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &envelope) != nil {
		return "", false
	}
	// A tool-call shape is never prose, even if a stray response key came
	// along (the flat rung permits both in one object).
	for _, key := range []string{"name", "tool", "tool_name", "function"} {
		if _, ok := envelope[key]; ok {
			return "", false
		}
	}
	raw, ok := envelope["response"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}
