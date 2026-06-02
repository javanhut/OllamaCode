package tui

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/javanhut/ollama_code/mcp"
)

func tc(name, args string) mcp.ToolCall {
	return mcp.ToolCall{Function: mcp.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestCallFingerprint_KeyOrderStable(t *testing.T) {
	a := callFingerprint(tc("edit_file", `{"path":"a","old_string":"x"}`))
	b := callFingerprint(tc("edit_file", `{"old_string":"x","path":"a"}`))
	if a != b {
		t.Fatalf("fingerprints should be key-order independent:\n%s\n%s", a, b)
	}
}

func TestCallFingerprint_DiffersByArgs(t *testing.T) {
	if callFingerprint(tc("read_file", `{"path":"a"}`)) == callFingerprint(tc("read_file", `{"path":"b"}`)) {
		t.Fatal("different args must produce different fingerprints")
	}
}

func TestIsOscillating(t *testing.T) {
	if !isOscillating([]string{"A", "B", "A", "B"}) {
		t.Fatal("ABAB should be detected as oscillating")
	}
	if isOscillating([]string{"A", "A", "A", "A"}) {
		t.Fatal("AAAA is repetition, not oscillation")
	}
	if isOscillating([]string{"A", "B", "C", "D"}) {
		t.Fatal("ABCD is progress, not oscillation")
	}
	if isOscillating([]string{"A", "B"}) {
		t.Fatal("too short to oscillate")
	}
}

func TestSalvageJSON(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},                                  // already valid: unchanged
		{"```json\n{\"a\":1}\n```", `{"a":1}`},                  // fenced
		{`{"a":1,}`, `{"a":1}`},                                 // trailing comma
		{"here you go: {\"path\":\"x\"} thanks", `{"path":"x"}`}, // surrounding prose
	}
	for _, c := range cases {
		got := string(salvageJSON(json.RawMessage(c.in)))
		if !json.Valid([]byte(got)) {
			t.Errorf("salvageJSON(%q) produced invalid JSON %q", c.in, got)
			continue
		}
		if got != c.want {
			t.Errorf("salvageJSON(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSalvageJSON_UnrepairableUnchanged(t *testing.T) {
	in := json.RawMessage(`not json at all`)
	if string(salvageJSON(in)) != string(in) {
		t.Fatal("unrepairable input must be returned unchanged")
	}
}

func TestRepairHint_ValidationError(t *testing.T) {
	call := tc("edit_file", `{"path":"a"}`)
	err := mcp.ValidateArgs(mcp.EditFileTool().Function, call.Function.Arguments)
	if err == nil {
		t.Fatal("expected validation error")
	}
	hint := repairHint(call, err)
	if !errorsContains(hint, "new_string") {
		t.Fatalf("repair hint should name the missing field: %s", hint)
	}
}

func TestRepairHint_BrokenJSON(t *testing.T) {
	call := tc("read_file", `{"path": broken`)
	hint := repairHint(call, errors.New("invalid arguments"))
	if !errorsContains(hint, "valid JSON") {
		t.Fatalf("expected broken-JSON guidance, got: %s", hint)
	}
}

func errorsContains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
