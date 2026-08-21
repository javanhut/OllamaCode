package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseAskUserArray(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"Which database?","options":["postgres","sqlite","  "]}`))
	if q.Question != "Which database?" {
		t.Fatalf("question: %q", q.Question)
	}
	if want := []string{"postgres", "sqlite"}; !reflect.DeepEqual(q.Options, want) {
		t.Fatalf("options: %#v want %#v", q.Options, want)
	}
}

// The old schema took a pipe-separated string, and a small model handed the
// array schema will still sometimes send one. Salvaging it saves a wasted turn.
func TestParseAskUserSalvagesPipeString(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"Proceed?","options":"yes|no|show me an example"}`))
	if want := []string{"yes", "no", "show me an example"}; !reflect.DeepEqual(q.Options, want) {
		t.Fatalf("options: %#v want %#v", q.Options, want)
	}
}

func TestParseAskUserMultiSelectAndRecommended(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"Which apply?","options":["a","b","c"],"multi_select":true,"recommended":"b"}`))
	if !q.MultiSelect {
		t.Fatal("multi_select was not parsed")
	}
	if q.Recommended != "b" {
		t.Fatalf("recommended: %q", q.Recommended)
	}
}

// A recommended label naming no option must be dropped, not rendered as a
// marker on a row that does not exist.
func TestParseAskUserDropsUnknownRecommended(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"Pick one","options":["a","b"],"recommended":"zzz"}`))
	if q.Recommended != "" {
		t.Fatalf("unknown recommended survived: %q", q.Recommended)
	}
}

// The salvaged pipe-separated form carries only the options — it stays
// single-select with no recommendation.
func TestParseAskUserPipeStringStaysSingleSelect(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"Proceed?","options":"yes|no"}`))
	if q.MultiSelect {
		t.Fatal("pipe-string salvage produced a multi-select question")
	}
	if q.Recommended != "" {
		t.Fatalf("pipe-string salvage produced a recommendation: %q", q.Recommended)
	}
}

func TestParseAskUserOpenQuestion(t *testing.T) {
	q := ParseAskUser(json.RawMessage(`{"question":"What should the timeout be?"}`))
	if len(q.Options) != 0 {
		t.Fatalf("expected no options, got %#v", q.Options)
	}
	// Malformed arguments must not panic or invent a question.
	if got := ParseAskUser(json.RawMessage(`not json`)); got.Question != "" || len(got.Options) != 0 {
		t.Fatalf("garbage arguments produced %#v", got)
	}
}

// The picker and the model must be shown the same options, so the handler's
// text is built from the same parse the TUI uses.
func TestAskUserHandlerListsParsedOptions(t *testing.T) {
	out, err := AskUserTool().Handler(context.Background(),
		json.RawMessage(`{"question":"Which database?","options":["postgres","sqlite"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"QUESTION: Which database?", "postgres | sqlite", "wait for the user"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
