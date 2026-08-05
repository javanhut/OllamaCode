package tools

import (
	"strings"
	"testing"
)

func TestReplacePrefixIsScoped(t *testing.T) {
	r := NewRegistry()
	r.Register(ReadFileTool())
	definition := Tool{Function: Function{Name: "mcp_demo_one", Parameters: Schema{Type: "object", Properties: map[string]Property{}}}}
	if err := r.ReplacePrefix("mcp_demo_", []Tool{definition}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("read_file"); !ok {
		t.Fatal("built-in was removed")
	}
	if _, ok := r.Lookup("mcp_demo_one"); !ok {
		t.Fatal("dynamic tool missing")
	}
	definition.Function.Name = "mcp_demo_two"
	if err := r.ReplacePrefix("mcp_demo_", []Tool{definition}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("mcp_demo_one"); ok {
		t.Fatal("old dynamic tool survived replacement")
	}
	if err := r.ReplacePrefix("mcp_demo_", []Tool{{Function: Function{Name: "outside"}}}); err == nil || !strings.Contains(err.Error(), "outside namespace") {
		t.Fatalf("expected namespace error, got %v", err)
	}
}
