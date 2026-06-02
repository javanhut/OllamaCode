package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/javanhut/ollama_code/mcp"
)

// Loop-safety tunables.
const (
	defaultMaxSteps     = 40 // tool-call rounds per user turn before we stop (room for verify-driven iteration)
	maxSameCallFailures = 2  // identical failing call attempts before short-circuit
	recentCallsKept     = 12 // fingerprint ring length for oscillation detection
)

func maxStepsFromConfig(c config) int {
	if c.MaxSteps > 0 {
		return c.MaxSteps
	}
	return defaultMaxSteps
}

// resetTurnGuards clears the per-turn loop-safety state. Call at the start of
// every new user turn (fresh submit or a dequeued message).
func (m *Model) resetTurnGuards() {
	m.stepCount = 0
	m.recentCalls = m.recentCalls[:0]
	m.oscillationWarned = false
	m.suppressToolsOnce = false
	m.lastStepTool = ""
	m.sameToolStreak = 0
	m.turnTouchedFiles = false
	m.verifyAttempts = 0
	m.challengedThisTurn = false
	for k := range m.failedCalls {
		delete(m.failedCalls, k)
	}
}

// batchSingleTool returns the tool name if every call in a batch is the same
// tool, else "". Used to detect a model spamming one tool (e.g. switch_mode)
// with varying arguments — which evades fingerprint-based repeat detection.
func batchSingleTool(calls []mcp.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	name := calls[0].Function.Name
	for _, c := range calls[1:] {
		if c.Function.Name != name {
			return ""
		}
	}
	return name
}

// canonicalJSON returns a stable serialization of a JSON value with object keys
// sorted (encoding/json marshals map keys in sorted order), so two semantically
// identical argument blobs produce the same fingerprint regardless of key order
// or whitespace. Falls back to the trimmed raw string when it isn't valid JSON.
func canonicalJSON(raw json.RawMessage) string {
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

// callFingerprint identifies a tool call by name + canonical arguments, so
// repeated and oscillating calls can be detected cheaply.
func callFingerprint(c mcp.ToolCall) string {
	return c.Function.Name + "\x00" + canonicalJSON(c.Function.Arguments)
}

// isOscillating reports whether the last four fingerprints form an A,B,A,B
// pattern — the model alternating between two actions without progress.
func isOscillating(recent []string) bool {
	n := len(recent)
	if n < 4 {
		return false
	}
	a, b, c, d := recent[n-4], recent[n-3], recent[n-2], recent[n-1]
	return a == c && b == d && a != b
}

var (
	codeFenceRe   = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*(.*?)\\s*```")
	trailingComma = regexp.MustCompile(`,(\s*[}\]])`)
)

// salvageJSON makes a conservative, best-effort attempt to repair almost-valid
// tool arguments emitted by weak models: it strips ```json fences, trims to the
// outermost {...}, and removes trailing commas. The repaired value is returned
// ONLY if it newly parses as valid JSON; otherwise the original is returned
// untouched. It never rewrites string contents, so it can't corrupt values.
func salvageJSON(raw json.RawMessage) json.RawMessage {
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

// repairHint turns a tool error into model-actionable feedback. Validation
// errors already render a named, schema-aware message; broken-JSON arguments get
// explicit guidance to resend a single object; everything else passes through.
func repairHint(call mcp.ToolCall, err error) string {
	var ve *mcp.ValidationError
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
