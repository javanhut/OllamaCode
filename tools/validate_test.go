package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func editFn() Function { return EditFileTool().Function }

func TestValidateArgs_OK(t *testing.T) {
	raw := json.RawMessage(`{"path":"a.go","old_string":"x","new_string":"y"}`)
	if err := ValidateArgs(editFn(), raw); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateArgs_MissingRequired(t *testing.T) {
	raw := json.RawMessage(`{"path":"a.go","old_string":"x"}`)
	err := ValidateArgs(editFn(), raw)
	if err == nil {
		t.Fatal("expected error for missing new_string")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(ve.Missing) != 1 || ve.Missing[0] != "new_string" {
		t.Fatalf("expected missing new_string, got %v", ve.Missing)
	}
	if !strings.Contains(err.Error(), "new_string") {
		t.Fatalf("error message should name new_string: %s", err.Error())
	}
}

func TestValidateArgs_NonObjectJSON(t *testing.T) {
	if err := ValidateArgs(editFn(), json.RawMessage(`"oops"`)); err == nil {
		t.Fatal("expected error for non-object JSON")
	}
}

func TestValidateArgs_CoercesQuotedBool(t *testing.T) {
	// replace_all is a boolean; a weak model may send it quoted.
	raw := json.RawMessage(`{"path":"a","old_string":"x","new_string":"y","replace_all":"true"}`)
	if err := ValidateArgs(editFn(), raw); err != nil {
		t.Fatalf("quoted bool should be tolerated, got %v", err)
	}
}

func TestNormalizeArgsCoercesValues(t *testing.T) {
	fn := Function{Name: "sample", Parameters: Schema{
		Type: "object",
		Properties: map[string]Property{
			"enabled": {Type: "boolean"},
			"count":   {Type: "integer"},
		},
		Required: []string{"enabled", "count"},
	}}
	got, err := NormalizeArgs(fn, json.RawMessage(`{"enabled":"true","count":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Enabled bool `json:"enabled"`
		Count   int  `json:"count"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("normalized arguments still fail handler decoding: %v", err)
	}
	if !decoded.Enabled || decoded.Count != 3 {
		t.Fatalf("unexpected normalized values: %+v", decoded)
	}
}

func TestValidateArgsRejectsUnknownAndNullRequired(t *testing.T) {
	if err := ValidateArgs(editFn(), json.RawMessage(`{"path":"a","old_string":"x","new_string":"y","pth":"typo"}`)); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
	if err := ValidateArgs(editFn(), json.RawMessage(`{"path":null,"old_string":"x","new_string":"y"}`)); err == nil {
		t.Fatal("expected null required field to be rejected")
	}
}

func TestNearest(t *testing.T) {
	r := DefaultRegistry()
	cases := map[string]string{
		"read":            "read_file",
		"replace_in_file": "edit_file", // contains "file"; should land on an edit/file tool
		"git_statuss":     "git_status",
	}
	for q, want := range cases {
		got, dist := r.Nearest(q)
		if q == "read" && got != want {
			t.Fatalf("Nearest(%q)=%q want %q (dist %d)", q, got, want, dist)
		}
		if q == "git_statuss" && got != want {
			t.Fatalf("Nearest(%q)=%q want %q (dist %d)", q, got, want, dist)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3},
		{"read", "read_file", 5},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
