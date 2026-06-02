package mcp

import "testing"

func firstCall(t *testing.T, calls []ToolCall) ToolCall {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("expected at least one parsed call")
	}
	return calls[0]
}

func TestParse_ToolCallTag(t *testing.T) {
	r := DefaultRegistry()
	c := firstCall(t, r.ParseToolCallsFromContent(`<tool_call>{"name": "read_file", "arguments": {"path": "a.go"}}</tool_call>`))
	if c.Function.Name != "read_file" {
		t.Fatalf("got %q", c.Function.Name)
	}
	if string(c.Function.Arguments) != `{"path": "a.go"}` {
		t.Fatalf("args: %s", c.Function.Arguments)
	}
}

func TestParse_FunctionTag(t *testing.T) {
	r := DefaultRegistry()
	c := firstCall(t, r.ParseToolCallsFromContent(`Sure: <function=list_directory>{"path": "."}</function>`))
	if c.Function.Name != "list_directory" {
		t.Fatalf("got %q", c.Function.Name)
	}
}

func TestParse_FencedJSON(t *testing.T) {
	r := DefaultRegistry()
	content := "```json\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\"}}\n```"
	c := firstCall(t, r.ParseToolCallsFromContent(content))
	if c.Function.Name != "grep" {
		t.Fatalf("got %q", c.Function.Name)
	}
}

func TestParse_BareWholeMessage(t *testing.T) {
	r := DefaultRegistry()
	c := firstCall(t, r.ParseToolCallsFromContent(`{"name":"get_working_directory","arguments":{}}`))
	if c.Function.Name != "get_working_directory" {
		t.Fatalf("got %q", c.Function.Name)
	}
}

func TestParse_ParametersKeyAndNestedFunction(t *testing.T) {
	r := DefaultRegistry()
	if c := firstCall(t, r.ParseToolCallsFromContent(`{"tool":"read_file","parameters":{"path":"x"}}`)); c.Function.Name != "read_file" {
		t.Fatalf("parameters key: got %q", c.Function.Name)
	}
	if c := firstCall(t, r.ParseToolCallsFromContent(`{"function":{"name":"read_file","arguments":{"path":"x"}}}`)); c.Function.Name != "read_file" {
		t.Fatalf("nested function: got %q", c.Function.Name)
	}
}

func TestParse_ArgumentsAsJSONString(t *testing.T) {
	r := DefaultRegistry()
	c := firstCall(t, r.ParseToolCallsFromContent(`{"name":"read_file","arguments":"{\"path\":\"x\"}"}`))
	if string(c.Function.Arguments) != `{"path":"x"}` {
		t.Fatalf("expected unwrapped args, got %s", c.Function.Arguments)
	}
}

// --- false positives must be rejected ---

func TestParse_RejectsUnregisteredName(t *testing.T) {
	r := DefaultRegistry()
	if got := r.ParseToolCallsFromContent(`{"name":"definitely_not_a_tool","arguments":{}}`); len(got) != 0 {
		t.Fatalf("unregistered name should not parse, got %v", got)
	}
}

func TestParse_RejectsProseWithExample(t *testing.T) {
	r := DefaultRegistry()
	prose := "To read a file you'd call read_file like this: `{\"name\":\"read_file\"}` — but I already have what I need, so here's the summary of the three files and the bug I found in the parser."
	if got := r.ParseToolCallsFromContent(prose); len(got) != 0 {
		t.Fatalf("prose mentioning a tool should not parse, got %v", got)
	}
}

func TestParse_RejectsFencedWithSurroundingProse(t *testing.T) {
	r := DefaultRegistry()
	content := "Here is a long explanation of what the tool does and why you might want to use it in various situations.\n```json\n{\"name\":\"grep\",\"arguments\":{}}\n```\nAnd here is even more explanation following the example block."
	if got := r.ParseToolCallsFromContent(content); len(got) != 0 {
		t.Fatalf("fenced example surrounded by prose should not parse, got %v", got)
	}
}

func TestParse_PlainTextReturnsNil(t *testing.T) {
	r := DefaultRegistry()
	if got := r.ParseToolCallsFromContent("I've finished the task. The fix is in place."); got != nil {
		t.Fatalf("plain text should return nil, got %v", got)
	}
}
