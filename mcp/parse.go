package mcp

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	toolCallTagRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	functionTagRe = regexp.MustCompile(`(?s)<function=([a-zA-Z0-9_.-]+)>\s*(.*?)\s*</function>`)
	jsonFenceRe   = regexp.MustCompile("(?s)```(?:json|tool_code|tool)?\\s*(.*?)\\s*```")
)

// ParseToolCallsFromContent extracts tool calls that a model emitted as TEXT
// rather than through the native tool-call channel (common with some Ollama
// templates for Qwen/Hermes/GLM). Only names registered in this registry are
// accepted, and loose JSON in prose is rejected, so ordinary assistant text is
// never hijacked. Recognizers, in priority order:
//
//  1. <tool_call>{...}</tool_call>      (explicit; always trusted)
//  2. <function=name>{...}</function>   (explicit; always trusted)
//  3. ```json {name, arguments} ```     (trusted only if it dominates the text)
//  4. a bare top-level JSON object/array that IS the entire message
func (r *Registry) ParseToolCallsFromContent(content string) []ToolCall {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}

	// 1 + 2: explicit tags.
	var tagged []ToolCall
	for _, m := range toolCallTagRe.FindAllStringSubmatch(content, -1) {
		tagged = append(tagged, r.parseCallObjects(m[1])...)
	}
	for _, m := range functionTagRe.FindAllStringSubmatch(content, -1) {
		if c, ok := r.toCallNamed(m[1], json.RawMessage(strings.TrimSpace(m[2]))); ok {
			tagged = append(tagged, c)
		}
	}
	if len(tagged) > 0 {
		return tagged
	}

	// 3: fenced JSON, only when it dominates the message (little surrounding
	// prose) so explanatory examples aren't executed.
	if fences := jsonFenceRe.FindAllStringSubmatch(content, -1); len(fences) > 0 {
		outside := strings.TrimSpace(jsonFenceRe.ReplaceAllString(content, ""))
		if len(outside) <= 60 {
			var calls []ToolCall
			for _, f := range fences {
				calls = append(calls, r.parseCallObjects(f[1])...)
			}
			if len(calls) > 0 {
				return calls
			}
		}
	}

	// 4: bare JSON that is the WHOLE message (no surrounding prose).
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		return r.parseCallObjects(trimmed)
	}
	return nil
}

// parseCallObjects parses s as a tool-call object or array of them.
func (r *Registry) parseCallObjects(s string) []ToolCall {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var arr []json.RawMessage
		if json.Unmarshal([]byte(s), &arr) == nil {
			var out []ToolCall
			for _, e := range arr {
				if c, ok := r.toCall(e); ok {
					out = append(out, c)
				}
			}
			return out
		}
	}
	if c, ok := r.toCall(json.RawMessage(s)); ok {
		return []ToolCall{c}
	}
	return nil
}

// toCall interprets a single JSON object as a tool call, tolerating the common
// key variations models use for the name and arguments.
func (r *Registry) toCall(raw json.RawMessage) (ToolCall, bool) {
	var m struct {
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		ToolName   string          `json:"tool_name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
		Args       json.RawMessage `json:"args"`
		Input      json.RawMessage `json:"input"`
		Function   struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ToolCall{}, false
	}
	name := firstNonEmpty(m.Function.Name, m.Name, m.Tool, m.ToolName)
	args := firstNonEmptyRaw(m.Function.Arguments, m.Arguments, m.Parameters, m.Args, m.Input)
	return r.toCallNamed(name, args)
}

// toCallNamed builds a ToolCall if name is a registered tool, normalizing the
// arguments (unwrapping a JSON-string-of-JSON, defaulting to {}).
func (r *Registry) toCallNamed(name string, args json.RawMessage) (ToolCall, bool) {
	if name == "" {
		return ToolCall{}, false
	}
	if _, ok := r.tools[name]; !ok {
		return ToolCall{}, false
	}
	args = bytesTrim(args)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	} else if s, ok := jsonStringPayload(args); ok && json.Valid([]byte(s)) {
		args = json.RawMessage(s)
	}
	return ToolCall{Function: ToolCallFunction{Name: name, Arguments: args}}, true
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func firstNonEmptyRaw(xs ...json.RawMessage) json.RawMessage {
	for _, x := range xs {
		if len(bytesTrim(x)) > 0 {
			return x
		}
	}
	return nil
}

func bytesTrim(b json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(b)))
}

// jsonStringPayload returns the unquoted contents when raw is a JSON string
// (e.g. arguments delivered as "{\"path\":\"x\"}"), so it can be re-parsed.
func jsonStringPayload(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if len(s) < 2 || s[0] != '"' {
		return "", false
	}
	var out string
	if json.Unmarshal(raw, &out) != nil {
		return "", false
	}
	return out, true
}
