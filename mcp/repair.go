package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	codeFenceRe   = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*(.*?)\\s*```")
	trailingComma = regexp.MustCompile(`,(\s*[}\]])`)
)

// SalvageJSON makes a conservative, best-effort attempt to repair almost-valid
// tool arguments emitted by weak models: it strips ```json fences, trims to the
// outermost {...}, and removes trailing commas. The repaired value is returned
// ONLY if it newly parses as valid JSON; otherwise the original is returned
// untouched. It never rewrites string contents, so it can't corrupt values.
func SalvageJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	s := string(raw)
	if m := codeFenceRe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			s = s[i : j+1]
		}
	}
	s = trailingComma.ReplaceAllString(s, "$1")
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return raw
}

// RepairHint turns a tool error into model-actionable feedback. Validation
// errors already render a named, schema-aware message; broken-JSON arguments get
// explicit guidance to resend a single object; everything else passes through.
func RepairHint(call ToolCall, err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return "error: " + ve.Error()
	}
	if len(call.Function.Arguments) > 0 && !json.Valid(call.Function.Arguments) {
		raw := string(call.Function.Arguments)
		if len(raw) > 300 {
			raw = raw[:300] + "…"
		}
		return fmt.Sprintf("error: arguments for %q were not valid JSON: %s\nResend ONLY a single JSON object with the tool's exact fields.", call.Function.Name, raw)
	}
	return fmt.Sprintf("error: %v. Check the arguments and try again.", err)
}

// ShouldFormatRepair reports whether a failed tool call looks like an ARGUMENT
// problem (bad schema or broken JSON) worth escalating to constrained decoding,
// as opposed to a legitimate execution error (e.g. "file not found") that
// re-emitting arguments wouldn't fix.
func ShouldFormatRepair(call ToolCall, err error) bool {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return true
	}
	return len(call.Function.Arguments) > 0 && !json.Valid(call.Function.Arguments)
}
