package tools

import (
	"encoding/json"
	"fmt"
	"slices"
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
	Unknown []string          // fields not declared by the schema
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
	if len(e.Unknown) > 0 {
		fmt.Fprintf(&b, " Unknown field(s): %s.", strings.Join(e.Unknown, ", "))
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
func ValidateArgs(fn Function, raw json.RawMessage) error {
	_, err := NormalizeArgs(fn, raw)
	return err
}

// NormalizeArgs validates arguments and converts quoted booleans and numbers
// into the concrete JSON types expected by handlers. Previously validation
// accepted those values but handlers then failed to unmarshal them.
func NormalizeArgs(fn Function, raw json.RawMessage) (json.RawMessage, error) {
	verr := &ValidationError{Tool: fn.Name, Fn: fn, Raw: raw}

	if len(strings.TrimSpace(string(raw))) == 0 {
		if len(fn.Parameters.Required) == 0 {
			return json.RawMessage(`{}`), nil
		}
		verr.Missing = append([]string(nil), fn.Parameters.Required...)
		return nil, verr
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		verr.JSONErr = err
		return nil, verr
	}

	for _, req := range fn.Parameters.Required {
		value, ok := fields[req]
		if !ok || jsonKind(value) == "null" {
			verr.Missing = append(verr.Missing, req)
		}
	}

	for name, val := range fields {
		prop, ok := fn.Parameters.Properties[name]
		if !ok {
			verr.Unknown = append(verr.Unknown, name)
			continue
		}
		normalized, valid := normalizeValue(prop, val)
		if !valid {
			if verr.Wrong == nil {
				verr.Wrong = map[string]string{}
			}
			verr.Wrong[name] = fmt.Sprintf("expected %s, got %s", prop.Type, jsonKind(val))
		} else {
			fields[name] = normalized
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

	if verr.JSONErr != nil || len(verr.Missing) > 0 || len(verr.Unknown) > 0 || len(verr.Wrong) > 0 || len(verr.BadEnum) > 0 {
		sort.Strings(verr.Unknown)
		return nil, verr
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return out, nil
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

func normalizeValue(prop Property, raw json.RawMessage) (json.RawMessage, bool) {
	kind := jsonKind(raw)
	if kind == "null" {
		return raw, true
	}
	switch prop.Type {
	case "", "any":
		return raw, true
	case "string":
		return raw, kind == "string"
	case "integer":
		if kind == "number" {
			return raw, true
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), true
	case "number":
		if kind == "number" {
			return raw, true
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatFloat(n, 'g', -1, 64)), true
	case "boolean":
		if kind == "boolean" {
			return raw, true
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		v, err := strconv.ParseBool(s)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatBool(v)), true
	case "object":
		return raw, kind == "object"
	case "array":
		if kind != "array" {
			return nil, false
		}
		if prop.Items == nil {
			return raw, true
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil, false
		}
		for i, value := range values {
			normalized, ok := normalizeValue(*prop.Items, value)
			if !ok {
				return nil, false
			}
			values[i] = normalized
		}
		out, err := json.Marshal(values)
		return out, err == nil
	default:
		return raw, true
	}
}

func contains(xs []string, s string) bool {
	return slices.Contains(xs, s)
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
		props[name] = propertySchema(p)
	}
	additional := false
	if f.Parameters.AdditionalProperties != nil {
		additional = *f.Parameters.AdditionalProperties
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": additional}
	if len(f.Parameters.Required) > 0 {
		schema["required"] = f.Parameters.Required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

func propertySchema(p Property) map[string]any {
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
	if p.Items != nil {
		entry["items"] = propertySchema(*p.Items)
	}
	if p.Type == "object" {
		properties := map[string]any{}
		for name, child := range p.Properties {
			properties[name] = propertySchema(child)
		}
		entry["properties"] = properties
		additional := false
		if p.AdditionalProperties != nil {
			additional = *p.AdditionalProperties
		}
		entry["additionalProperties"] = additional
		if len(p.Required) > 0 {
			entry["required"] = p.Required
		}
	}
	return entry
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
