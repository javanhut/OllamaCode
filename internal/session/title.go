package session

import (
	"strings"

	"github.com/javanhut/ollama_code/api"
)

const (
	// fallbackTitleWords bounds the deterministic title to roughly the first
	// clause of the user's opening message.
	fallbackTitleWords = 8
	// maxTitleRunes caps any title — fallback, generated, or user-set — so the
	// /sessions list stays one line per session. Runes, not bytes: a title in
	// a multibyte script gets the same character budget.
	maxTitleRunes = 60
)

// FallbackTitle derives a session title from the first thing the user actually
// typed: whitespace (newlines included) collapsed to single spaces, capped at
// fallbackTitleWords words and maxTitleRunes runes. Advisories ride the user
// role but are the harness talking, so they don't count. Empty when no message
// yields usable text.
func FallbackTitle(messages []api.Message) string {
	for _, msg := range messages {
		if msg.Role != "user" || msg.Advisory {
			continue
		}
		if text := collapseWhitespace(msg.Content); text != "" {
			return truncateTitle(text, fallbackTitleWords)
		}
	}
	return ""
}

// SanitizeTitle normalizes a model- or user-supplied title: first line only,
// whitespace collapsed, surrounding quotes stripped, capped at maxTitleRunes
// runes. Returns "" when nothing usable remains, so callers can keep whatever
// title they already had.
func SanitizeTitle(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = collapseWhitespace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSpace(s)
	return truncateRunes(s, maxTitleRunes)
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateTitle(s string, maxWords int) string {
	if words := strings.Fields(s); len(words) > maxWords {
		s = strings.Join(words[:maxWords], " ")
	}
	return truncateRunes(s, maxTitleRunes)
}

// truncateRunes cuts s to at most n runes on a rune boundary, never mid-UTF-8
// sequence, and trims the dangling partial word's trailing space.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}
