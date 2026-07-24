package tui

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestColorizeDiffGutter(t *testing.T) {
	diff := "--- f.txt\n+++ f.txt\n@@ -2,4 +2,4 @@\n 2\n 3\n-x\n+y\n 6"
	got := ansiRe.ReplaceAllString(colorizeDiff(diff, 40), "")
	// Deletions carry the old file's number, everything else the new file's.
	want := []string{"   2  2", "   3  3", "   4 -x", "   4 +y", "   5  6"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing gutter line %q in:\n%s", w, got)
		}
	}
	if !strings.Contains(got, "     --- f.txt") {
		t.Errorf("file header should have a blank gutter:\n%s", got)
	}
}
