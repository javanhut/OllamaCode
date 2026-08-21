package session

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/javanhut/ollama_code/api"
)

func TestFallbackTitle(t *testing.T) {
	cases := []struct {
		name     string
		messages []api.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: nil,
			want:     "",
		},
		{
			name:     "no user message",
			messages: []api.Message{{Role: "assistant", Content: "hi there"}},
			want:     "",
		},
		{
			name: "first user message wins",
			messages: []api.Message{
				{Role: "user", Content: "fix the login bug"},
				{Role: "user", Content: "and the signup one too"},
			},
			want: "fix the login bug",
		},
		{
			name: "whitespace collapses, newlines gone",
			messages: []api.Message{
				{Role: "user", Content: "fix the\n\n  login\t bug   please"},
			},
			want: "fix the login bug please",
		},
		{
			name: "empty user messages are skipped",
			messages: []api.Message{
				{Role: "user", Content: "   \n "},
				{Role: "user", Content: "real question"},
			},
			want: "real question",
		},
		{
			name: "advisories are not the user",
			messages: []api.Message{
				{Role: "user", Content: "[LOOP BROKEN] stop", Advisory: true},
				{Role: "user", Content: "actual request"},
			},
			want: "actual request",
		},
		{
			name: "word cap",
			messages: []api.Message{
				{Role: "user", Content: "one two three four five six seven eight nine ten"},
			},
			want: "one two three four five six seven eight",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FallbackTitle(tc.messages); got != tc.want {
				t.Errorf("FallbackTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// A long message is cut on a rune boundary: the result must be valid UTF-8 of
// at most maxTitleRunes runes even when every character is multibyte.
func TestFallbackTitleRuneSafety(t *testing.T) {
	long := strings.Repeat("界", maxTitleRunes+20)
	got := FallbackTitle([]api.Message{{Role: "user", Content: long}})
	if !utf8.ValidString(got) {
		t.Fatal("fallback title is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n > maxTitleRunes {
		t.Fatalf("fallback title = %d runes, want <= %d", n, maxTitleRunes)
	}
	// Single words over the rune cap truncate rather than vanish.
	if got == "" {
		t.Fatal("fallback title empty for a long single word")
	}
}

func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   \n ", ""},
		{"fix the login bug", "fix the login bug"},
		{"  spaced   out  title ", "spaced out title"},
		{"first line\nsecond line", "first line"},
		{"carriage\r\nreturn", "carriage"},
		{"\"quoted title\"", "quoted title"},
		{"'single quoted'", "single quoted"},
		{"`backticked`", "backticked"},
		{strings.Repeat("a", maxTitleRunes+10), strings.Repeat("a", maxTitleRunes)},
	}
	for _, tc := range cases {
		if got := SanitizeTitle(tc.in); got != tc.want {
			t.Errorf("SanitizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeTitleRuneSafety(t *testing.T) {
	got := SanitizeTitle(strings.Repeat("タ", maxTitleRunes+5))
	if !utf8.ValidString(got) {
		t.Fatal("sanitized title is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != maxTitleRunes {
		t.Fatalf("sanitized title = %d runes, want %d", n, maxTitleRunes)
	}
}

// SaveTo fills an empty title from the session's own first user message; an
// existing title (generated or pinned) is never rewritten.
func TestSaveToDerivesFallbackTitle(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/s.json"
	s := Session{
		Name:     "s",
		Messages: []api.Message{{Role: "user", Content: "debug the   parser"}},
	}
	if err := SaveTo(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "debug the parser" {
		t.Fatalf("title = %q, want the derived fallback", got.Title)
	}
	if got.TitlePinned {
		t.Fatal("a derived title must not be pinned")
	}

	got.Title = "better title"
	if err := SaveTo(path, *got); err != nil {
		t.Fatal(err)
	}
	again, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Title != "better title" {
		t.Fatalf("existing title overwritten with %q", again.Title)
	}
}
