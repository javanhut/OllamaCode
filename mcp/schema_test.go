package mcp

import (
	"encoding/json"
	"testing"
)

func TestJSONSchema_EditFile(t *testing.T) {
	raw := EditFileTool().Function.JSONSchema()
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("JSONSchema produced invalid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("expected object type, got %q", schema.Type)
	}
	for _, want := range []string{"path", "old_string", "new_string", "replace_all", "start_line", "end_line"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("missing property %q in schema", want)
		}
	}
	if len(schema.Required) != 2 {
		t.Fatalf("expected 2 required fields, got %v", schema.Required)
	}
}

func TestLookup(t *testing.T) {
	r := DefaultRegistry()
	if _, ok := r.Lookup("read_file"); !ok {
		t.Fatal("read_file should be registered")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unknown tool should not be found")
	}
}
