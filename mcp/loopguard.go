package mcp

import (
	"encoding/json"
	"strings"
)

// CanonicalJSON returns a stable serialization of a JSON value with object keys
// sorted (encoding/json marshals map keys in sorted order), so two semantically
// identical argument blobs produce the same fingerprint regardless of key order
// or whitespace. Falls back to the trimmed raw string when it isn't valid JSON.
func CanonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}

// CallFingerprint identifies a tool call by name + canonical arguments, so
// repeated and oscillating calls can be detected cheaply. Shared by the TUI loop
// and the headless sub-agent loop.
func CallFingerprint(c ToolCall) string {
	return c.Function.Name + "\x00" + CanonicalJSON(c.Function.Arguments)
}

// DedupeCalls drops exact duplicates (same tool, same canonical arguments) from
// a single batch. Weak models sometimes emit one call twice; running a mutating
// tool twice fails or double-applies, and even a read run twice wastes a slot.
// Results are matched to calls by tool name, so dropping a dup is safe. Shared
// by the TUI loop and the headless sub-agent loop.
func DedupeCalls(calls []ToolCall) []ToolCall {
	if len(calls) < 2 {
		return calls
	}
	seen := make(map[string]bool, len(calls))
	out := calls[:0]
	for _, c := range calls {
		fp := CallFingerprint(c)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, c)
	}
	return out
}

// IsOscillating reports whether the last four fingerprints form an A,B,A,B
// pattern — the model alternating between two actions without making progress.
func IsOscillating(recent []string) bool {
	n := len(recent)
	if n < 4 {
		return false
	}
	a, b, c, d := recent[n-4], recent[n-3], recent[n-2], recent[n-1]
	return a == c && b == d && a != b
}
