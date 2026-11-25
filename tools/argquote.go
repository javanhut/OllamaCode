package tools

import (
	"encoding/json"
	"strings"
)

// Some runtimes hand back tool arguments with string values still wrapped in
// their own escaped quotes: {"path": "\"fact.txt\""} — observed with gemma4 on
// Ollama under schema-constrained decoding, where every call then fails on a
// path that does not exist and the model gives up (cmd/eval: -67 points on
// gemma4 -constrain before this fix). The executor normalizes them.

// pathLikeArgs are argument names whose value is never legitimately wrapped in
// literal double quotes, so a bare "x" pair there is unwrapped too.
var pathLikeArgs = map[string]bool{
	"path": true, "dest": true, "destination": true, "new_path": true, "to": true,
	"source": true, "src": true, "working_dir": true, "dir": true, "directory": true,
	"file": true, "filename": true, "url": true,
}

// UnwrapQuotedArgs strips doubled quoting from top-level string arguments:
// an escaped pair (\"x\") anywhere, and a bare quote pair ("x") on path-like
// arguments only — file content that starts and ends with a quote is left
// alone. Returns args unchanged when nothing applies or it is not an object.
func UnwrapQuotedArgs(args json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args
	}
	changed := false
	for key, raw := range obj {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		unwrapped, ok := unwrapQuoted(s, pathLikeArgs[key])
		if !ok {
			continue
		}
		enc, err := json.Marshal(unwrapped)
		if err != nil {
			continue
		}
		obj[key] = enc
		changed = true
	}
	if !changed {
		return args
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return out
}

func unwrapQuoted(s string, bareToo bool) (string, bool) {
	if len(s) >= 4 && strings.HasPrefix(s, `\"`) && strings.HasSuffix(s, `\"`) {
		inner := s[2 : len(s)-2]
		var decoded string
		if json.Unmarshal([]byte(`"`+inner+`"`), &decoded) == nil {
			return decoded, true
		}
		return strings.ReplaceAll(inner, `\"`, `"`), true
	}
	if bareToo && len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `"`) {
		return s[1 : len(s)-1], true
	}
	return "", false
}
