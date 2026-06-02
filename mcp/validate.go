package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ValidationError describes why a tool call's arguments failed validation. Its
// Error() renders a compact, model-actionable correction so a weak model can
// self-repair on the next turn rather than dead-ending.
type ValidationError struct {
	Tool    string
	JSONErr error             // non-nil if the raw arguments aren't a JSON object
	Missing []string          // required fields that are absent
	Wrong   map[string]string // field -> "expected X, got Y"
	BadEnum map[string]string // field -> "must be one of [...]"
	Fn      Function          // for rendering the expected shape
	Raw     json.RawMessage   // what the model actually sent
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: invalid arguments.", e.Tool)
	if e.JSONErr != nil {
		fmt.Fprintf(&b, " Arguments were not a valid JSON object (%v).", e.JSONErr)
	}
	if len(e.Missing) > 0 {
		fmt.Fprintf(&b, " Missing required field(s): %s.", strings.Join(e.Missing, ", "))
	}
	for f, m := range e.Wrong {
		fmt.Fprintf(&b, " Field %q: %s.", f, m)
	}
	for f, m := range e.BadEnum {
		fmt.Fprintf(&b, " Field %q: %s.", f, m)
	}
	fmt.Fprintf(&b, "\nExpected arguments: %s", e.Fn.argShape())
	if len(e.Raw) > 0 {
		raw := string(e.Raw)
		if len(raw) > 400 {
			raw = raw[:400] + "…"
		}
		fmt.Fprintf(&b, "\nYou sent: %s", raw)
	}
	return b.String()
}

// argShape renders a one-line, human/model-readable description of a tool's
// expected argument object, e.g. {"path": string, "old_string": string, "replace_all"?: boolean}.
func (f Function) argShape() string {
	names := make([]string, 0, len(f.Parameters.Properties))
	for n := range f.Parameters.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	required := map[string]bool{}
	for _, r := range f.Parameters.Required {
		required[r] = true
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		p := f.Parameters.Properties[n]
		key := n
		if !required[n] {
			key += "?"
		}
		typ := p.Type
		if len(p.Enum) > 0 {
			typ = `"` + strings.Join(p.Enum, `"|"`) + `"`
		}
		parts = append(parts, fmt.Sprintf("%q: %s", key, typ))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ValidateArgs checks a tool call's raw arguments against the tool's schema.
// It is intentionally lenient on types (weak models often send a number as a
// string) and only hard-fails on non-object JSON, missing required fields, and
// clearly-wrong enum values — the cheap, high-frequency mistakes. Handlers keep
// owning their own deep validation.
func ValidateArgs(fn Function, raw json.RawMessage) error {
	verr := &ValidationError{Tool: fn.Name, Fn: fn, Raw: raw}

	// Empty args are OK only if there are no required fields.
	if len(strings.TrimSpace(string(raw))) == 0 {
		if len(fn.Parameters.Required) == 0 {
			return nil
		}
		verr.Missing = append([]string(nil), fn.Parameters.Required...)
		return verr
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		verr.JSONErr = err
		return verr
	}

	for _, req := range fn.Parameters.Required {
		if _, ok := fields[req]; !ok {
			verr.Missing = append(verr.Missing, req)
		}
	}

	for name, val := range fields {
		prop, ok := fn.Parameters.Properties[name]
		if !ok {
			continue // unknown field: ignore, don't fail
		}
		if !typeLooseMatch(prop.Type, val) {
			if verr.Wrong == nil {
				verr.Wrong = map[string]string{}
			}
			verr.Wrong[name] = fmt.Sprintf("expected %s, got %s", prop.Type, jsonKind(val))
		}
		if len(prop.Enum) > 0 {
			var s string
			if json.Unmarshal(val, &s) == nil && !contains(prop.Enum, s) {
				if verr.BadEnum == nil {
					verr.BadEnum = map[string]string{}
				}
				verr.BadEnum[name] = "must be one of [" + strings.Join(prop.Enum, ", ") + "]"
			}
		}
	}

	if verr.JSONErr != nil || len(verr.Missing) > 0 || len(verr.Wrong) > 0 || len(verr.BadEnum) > 0 {
		return verr
	}
	return nil
}

// jsonKind reports the JSON kind of a raw value by peeking the first byte.
func jsonKind(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "empty"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// typeLooseMatch reports whether a raw JSON value is acceptable for a schema
// type. It is coercion-friendly: a quoted number/bool is accepted where a
// number/boolean is expected, since weak models frequently over-quote.
func typeLooseMatch(schemaType string, raw json.RawMessage) bool {
	kind := jsonKind(raw)
	if kind == "null" {
		return true // treat null as "absent-ish"; let the handler decide
	}
	switch schemaType {
	case "", "any":
		return true
	case "string":
		return kind == "string"
	case "number", "integer":
		if kind == "number" {
			return true
		}
		// accept a numeric string
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if _, err := strconv.ParseFloat(s, 64); err == nil {
				return true
			}
		}
		return false
	case "boolean":
		if kind == "boolean" {
			return true
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if _, err := strconv.ParseBool(s); err == nil {
				return true
			}
		}
		return false
	case "object":
		return kind == "object"
	case "array":
		return kind == "array"
	default:
		return true
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Lookup returns the registered tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// JSONSchema renders the tool's parameter schema as a JSON-schema object,
// suitable for Ollama's `format` field (constrained decoding of the arguments).
func (f Function) JSONSchema() json.RawMessage {
	props := map[string]any{}
	for name, p := range f.Parameters.Properties {
		entry := map[string]any{}
		if p.Type != "" {
			entry["type"] = p.Type
		}
		if p.Description != "" {
			entry["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			entry["enum"] = p.Enum
		}
		props[name] = entry
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(f.Parameters.Required) > 0 {
		schema["required"] = f.Parameters.Required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Nearest returns the registered tool name closest to the given (typically
// hallucinated) name, along with a confidence distance (lower = closer). It
// rewards prefix/substring overlap so a short invented name like "read" maps to
// "read_file" rather than a same-length but unrelated tool. The returned dist is
// the adjusted score and is what callers should threshold on.
func (r *Registry) Nearest(name string) (best string, dist int) {
	lname := strings.ToLower(name)
	best, dist = "", 1<<30
	for _, n := range r.Names() {
		ln := strings.ToLower(n)
		score := levenshtein(lname, ln)
		switch {
		case strings.HasPrefix(ln, lname) || strings.HasPrefix(lname, ln):
			score -= 4
		case strings.Contains(ln, lname) || strings.Contains(lname, ln):
			score -= 2
		}
		if score < dist {
			dist, best = score, n
		}
	}
	return best, dist
}

// levenshtein computes the edit distance between two strings (two-row DP).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
