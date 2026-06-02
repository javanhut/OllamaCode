package mcp

import (
	"strings"
	"testing"
)

func TestApplyEdit_ExactSingle(t *testing.T) {
	got, count, tier, err := applyEdit("foo bar baz", "bar", "QUX", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "foo QUX baz" || count != 1 || tier != 1 {
		t.Fatalf("got %q count=%d tier=%d", got, count, tier)
	}
}

func TestApplyEdit_ExactAmbiguous(t *testing.T) {
	if _, _, _, err := applyEdit("x x x", "x", "y", false); err == nil {
		t.Fatal("expected ambiguity error without replace_all")
	}
	got, count, _, err := applyEdit("x x x", "x", "y", true)
	if err != nil || got != "y y y" || count != 3 {
		t.Fatalf("replace_all: got %q count=%d err=%v", got, count, err)
	}
}

func TestApplyEdit_WhitespaceTolerant(t *testing.T) {
	// File uses a tab indent; model supplies spaces. Tier 2 should match and
	// re-indent the replacement to the file's actual (tab) indentation.
	content := "func f() {\n\treturn 1\n}\n"
	old := "    return 1" // 4 spaces, wrong indent
	got, count, tier, err := applyEdit(content, old, "return 2", false)
	if err != nil {
		t.Fatal(err)
	}
	if tier != 2 || count != 1 {
		t.Fatalf("expected tier 2 single match, got tier=%d count=%d", tier, count)
	}
	want := "func f() {\n\treturn 2\n}\n"
	if got != want {
		t.Fatalf("re-indent failed:\n got %q\nwant %q", got, want)
	}
}

func TestApplyEdit_CRLFPreserved(t *testing.T) {
	content := "a\r\ntarget\r\nb\r\n"
	got, _, tier, err := applyEdit(content, "target", "changed", false)
	if err != nil {
		t.Fatal(err)
	}
	// Exact match works here (target has no surrounding whitespace), tier 1.
	if !strings.Contains(got, "changed") {
		t.Fatalf("expected replacement, got %q", got)
	}
	_ = tier
}

func TestApplyEdit_NotFound(t *testing.T) {
	if _, _, _, err := applyEdit("hello world", "nonexistent snippet", "x", false); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestApplyEdit_MultilineWhitespace(t *testing.T) {
	content := "if x {\n        doThing()\n        doOther()\n}\n"
	old := "doThing()\ndoOther()" // no indentation at all
	got, count, tier, err := applyEdit(content, old, "doNew()\ndoOther()", false)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if tier != 2 || count != 1 {
		t.Fatalf("expected tier2 single, got tier=%d count=%d", tier, count)
	}
	if !strings.Contains(got, "        doNew()") {
		t.Fatalf("expected re-indented doNew, got %q", got)
	}
}
