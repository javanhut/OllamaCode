package tools

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

func TestJSONSchemaPreservesNestedConstraints(t *testing.T) {
	fn := Function{Name: "nested", Parameters: Schema{
		Type: "object",
		Properties: map[string]Property{
			"items": {
				Type: "array",
				Items: &Property{Type: "object", Properties: map[string]Property{
					"name": {Type: "string"},
				}, Required: []string{"name"}},
			},
		},
	}}
	var schema map[string]any
	if err := json.Unmarshal(fn.JSONSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("top-level schema should reject additional properties")
	}
	items := schema["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatal("nested object schema should reject additional properties")
	}
	if len(items["required"].([]any)) != 1 {
		t.Fatal("nested required fields were not preserved")
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
