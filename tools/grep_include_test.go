package tools

import "testing"

func TestIncludeGlob(t *testing.T) {
	cases := map[string]string{
		".go":       "*.go", // bare extension with dot — the case that broke grep
		"go":        "*.go", // bare extension without dot
		"*.go":      "*.go", // already a glob — untouched
		"*_test.go": "*_test.go",
		"[Mm]ake*":  "[Mm]ake*",
	}
	for in, want := range cases {
		if got := includeGlob(in); got != want {
			t.Errorf("includeGlob(%q) = %q, want %q", in, got, want)
		}
	}
}
