package tools

import (
	"fmt"
	"os"
	"path/filepath"
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

// A preview with a side effect would be worse than the bug it fixes: the
// permission modal runs PreviewEdit before the user has approved anything.
func TestPreviewEdit_DoesNotTouchDisk(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	path := filepath.Join(root, "sample.txt")
	before := "func f() {\n\treturn 1\n}\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	// Tier 2: the model's old_string is indented with spaces, the file uses a tab.
	diff, ok := PreviewEdit(path, jailArgs(t, map[string]any{
		"path": path, "old_string": "    return 1", "new_string": "return 2",
	}))
	if !ok {
		t.Fatal("expected the edit to resolve")
	}
	if !strings.Contains(diff, "-\treturn 1") {
		t.Fatalf("preview should show the file's real tab-indented line:\n%s", diff)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("PreviewEdit wrote to disk:\n got %q\nwant %q", after, before)
	}
}

// A 6000-line file used to blow past unifiedDiff's size guard, so the approval
// modal showed only "(diff omitted: file too large)" — less than the
// claim-based preview it replaced.
func TestPreviewEdit_LargeFile(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	path := filepath.Join(root, "huge.txt")
	lines := make([]string, 6000)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, ok := PreviewEdit(path, jailArgs(t, map[string]any{
		"path": path, "old_string": "line 3000", "new_string": "LINE THREE THOUSAND",
	}))
	if !ok {
		t.Fatal("expected the edit to resolve")
	}
	if !strings.Contains(diff, "-line 3000") || !strings.Contains(diff, "+LINE THREE THOUSAND") {
		t.Fatalf("preview should show the replaced line:\n%s", diff)
	}
	if !strings.Contains(diff, "@@ -2998,") {
		t.Fatalf("hunk header should keep the file's real line numbers:\n%s", diff)
	}
}

// A minified file: one huge line among many short ones. The model's old_string
// is a fragment of that line with one stale character, so no whole-line matcher
// can hit it — tier 3 used to report an unrelated <div class="bars"> line 1200
// lines away, and the model would loop retrying against that garbage.
func TestApplyEdit_FragmentOfMinifiedLine(t *testing.T) {
	long := `()=>{agentDone(3,'3 matches');addStep('<span class="tool ss">semantic_search</span><div class="pct">79%</div></div>'` +
		strings.Repeat(`;noop("padpadpadpad")`, 120) + `}`
	var b strings.Builder
	for range 1000 {
		b.WriteString(`  <div class="bars"><i></i><i></i><i></i><i></i></div>` + "\n")
	}
	b.WriteString("  " + long + "\n")
	content := b.String()

	old := `addStep('<span class="tool ss">semantic_search</span><div class="pct">79%</div></div>)` // stale: ) for '
	got, count, tier, err := applyEdit(content, old, `addStep('FIXED')`, false)
	if err != nil {
		t.Fatalf("tier %d refused a fragment of a long line: %v", tier, err)
	}
	if count != 1 || !strings.Contains(got, `addStep('FIXED')`) {
		t.Fatalf("count=%d got line %q", count, got[strings.LastIndex(got, "\n  ()=>"):])
	}
	if strings.Contains(got, "semantic_search") || !strings.Contains(got, `agentDone(3,'3 matches')`) {
		t.Fatal("spliced the wrong span: rest of the minified line must survive")
	}
	if n := strings.Count(got, "\n"); n != strings.Count(content, "\n") {
		t.Fatalf("line count changed: %d != %d", n, strings.Count(content, "\n"))
	}
}

// An ambiguous fragment must refuse rather than splice a coin-flip winner.
func TestApplyEdit_FragmentAmbiguous(t *testing.T) {
	frag := `renderStep("alpha",{pct:79,label:"searching the index"});`
	content := "a{" + frag + "}\nb{" + frag + "}\n"
	if _, _, _, err := applyEdit(content, strings.Replace(frag, "79", "80", 1), "X", false); err == nil {
		t.Fatal("expected refusal for a fragment that matches two spans equally")
	}
}
