package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Lazy placeholders: a model rewriting code sometimes elides what it didn't
// change ("// ... rest of the file unchanged", "# existing code ...") instead
// of writing it out. Written to disk, the comment replaces the real code.
// Aider added its unified-diff format largely to stop this; a whole-file
// rewrite, which the edit fallback suggests, is where it does most harm.

// commentOpen and commentClose strip a comment line down to its text.
var (
	commentOpen  = regexp.MustCompile(`^\s*(//+|#+|/\*+|\*+|<!--|--|;+)\s*`)
	commentClose = regexp.MustCompile(`\s*(\*+/|-->)\s*$`)
)

// elisionWords are what a placeholder says alongside an ellipsis.
var elisionWords = regexp.MustCompile(`(?i)\b(rest|remainder|remaining|existing|unchanged|previous|original|other|same|omitted|elided|etc|as before|code|here)\b`)

// elisionPhrase is a whole-comment placeholder written without an ellipsis.
// Anchored at both ends so a real comment that merely starts the same way
// ("Other methods are defined in fs.go") is left alone.
var elisionPhrase = regexp.MustCompile(`(?i)^(` +
	`(the\s+)?(rest|remainder)\s+of\s+(the\s+)?(file|code|class|function|method|module|implementation|component|struct|body)(\s+(is\s+|remains\s+)?(unchanged|the\s+same|as\s+before))?` +
	`|(existing|remaining|previous|original|other)\s+(code|methods|functions|imports|content|implementation|logic|tests|fields|cases)(\s+(here|unchanged|remains?\s+unchanged|stays?\s+the\s+same|as\s+before|goes\s+here))?` +
	`|unchanged|no\s+changes(\s+(here|below|above))?` +
	`)\s*[.:]?$`)

// placeholderLine reports whether line is a comment that stands in for code.
func placeholderLine(line string, pythonic bool) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "..." || trimmed == "…" {
		return !pythonic // a bare "..." is Python's Ellipsis, a real stub body
	}
	loc := commentOpen.FindStringIndex(line)
	if loc == nil {
		return false
	}
	body := strings.TrimSpace(commentClose.ReplaceAllString(line[loc[1]:], ""))
	if strings.Contains(body, "...") || strings.Contains(body, "…") {
		words := strings.TrimSpace(strings.NewReplacer("...", " ", "…", " ").Replace(body))
		return len(body) <= 80 && (words == "" || elisionWords.MatchString(words))
	}
	return elisionPhrase.MatchString(body)
}

// findPlaceholder returns the first line of text that looks like a lazy
// placeholder and does not already appear in original, or "" if none does. A
// line already in the file is real content (a stub, a doc comment) and passes.
// Prose files are skipped: a Markdown heading is not a comment.
func findPlaceholder(path, text, original string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown", ".txt", ".rst", ".adoc":
		return ""
	}
	pythonic := ext == ".py" || ext == ".pyi"
	var existing map[string]bool
	for line := range strings.SplitSeq(text, "\n") {
		if !placeholderLine(line, pythonic) {
			continue
		}
		if existing == nil {
			existing = map[string]bool{}
			for l := range strings.SplitSeq(original, "\n") {
				existing[strings.TrimSpace(l)] = true
			}
		}
		if trimmed := strings.TrimSpace(line); !existing[trimmed] {
			return trimmed
		}
	}
	return ""
}

// placeholderError explains a rejected write or edit.
func placeholderError(path, line string) error {
	return fmt.Errorf("rejected: the new text for %s contains the placeholder %q, which would replace real code with a comment. Write out the actual code in full; nothing was changed", path, line)
}
