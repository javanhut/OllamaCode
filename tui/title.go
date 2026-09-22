package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// Session titles (ported from opencode). Every save already carries a
// deterministic fallback derived from the first user message
// (session.FallbackTitle, applied in session.SaveTo); on top of that, the
// first completed assistant reply of a conversation fires exactly one
// background request asking the model for something better. The generated
// title only ever replaces an unpinned one — /title pins.
//
// There is no separate small-model config to prefer, so like dream mode the
// call runs only against the default host: a routed provider is a metered API
// and a nicer label in /sessions is not worth spending it. Skips and failures
// are silent — the fallback title stays.

type titleDoneMsg struct {
	title string // sanitized; empty means the attempt failed — keep the fallback
}

const titleSystemPrompt = `You name conversations. Reply with a title of at most 6 words for the user's opening message: plain words only, no quotes, no punctuation, no prefix, nothing else.`

// titleExcerptRunes bounds how much of the opening message goes into the title
// prompt — the model only needs the gist.
const titleExcerptRunes = 400

// maybeTitleCmd returns the one-shot title-generation command after a
// conversation's first completed turn, or nil. Called from endTurnTail, so it
// never disturbs the stream: the request runs in a tea.Cmd goroutine and the
// result lands as a titleDoneMsg like any other async completion.
func (m *Model) maybeTitleCmd() tea.Cmd {
	if m.titleGenTried || m.titlePinned || m.modelName == "" {
		return nil
	}
	excerpt := firstUserText(m.history)
	if excerpt == "" {
		return nil
	}
	// Latch before the host check: the first reply happens once, so the chance
	// to generate does too, even when this turn ran on a provider.
	m.titleGenTried = true
	if m.host.URL() != m.cfg.Host {
		return nil // routed provider: not worth the metered call
	}
	if r := []rune(excerpt); len(r) > titleExcerptRunes {
		excerpt = string(r[:titleExcerptRunes])
	}
	host, model := m.host, m.modelName
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := host.ChatOnce(ctx, api.ChatRequest{
			Model: model,
			Messages: []api.Message{
				{Role: "system", Content: titleSystemPrompt},
				{Role: "user", Content: excerpt},
			},
			Options: map[string]any{"num_ctx": 2048, "temperature": 0.2, "num_predict": 24},
		})
		if err != nil {
			return titleDoneMsg{}
		}
		return titleDoneMsg{title: session.SanitizeTitle(resp.Message.Content)}
	}
}

// applyGeneratedTitle stores a finished title and re-saves. Runs in the UI
// loop; a pinned title (set via /title while the request was in flight) wins.
func (m *Model) applyGeneratedTitle(msg titleDoneMsg) {
	if msg.title == "" || m.titlePinned {
		return
	}
	m.sessionTitle = msg.title
	m.autosaveSession()
}

// titleCommand: /title <text> — set the session title by hand, which pins it
// against the generator. Carried by the auto-save from here on, and written
// through to the named session this conversation was loaded from or saved as.
func (m *Model) titleCommand(text string) {
	title := session.SanitizeTitle(text)
	if title == "" {
		m.toast = "usage: /title <text>"
		return
	}
	m.sessionTitle, m.titlePinned = title, true
	if m.sessionName != "" {
		if s, err := session.Load(m.sessionName); err == nil {
			s.Title, s.TitlePinned = title, true
			_ = session.Save(*s) // best-effort, like every session write
		}
	}
	m.autosaveSession()
	m.toast = "session title: " + title
}

// renameCommand: /rename <new-name> — rename the saved session this
// conversation belongs to. There is nothing to rename until /save or /load
// gives the conversation a name.
func (m *Model) renameCommand(name string) {
	if name == "" {
		m.toast = "usage: /rename <new-name>"
		return
	}
	if m.sessionName == "" {
		m.toast = "no saved session — /save <name> first, then /rename"
		return
	}
	if name == m.sessionName {
		return
	}
	s, err := session.Load(m.sessionName)
	if err != nil {
		m.toast = "rename failed: " + err.Error()
		return
	}
	s.Name = name
	s.UpdatedAt = time.Now()
	if err := session.Save(*s); err != nil {
		m.toast = "rename failed: " + err.Error()
		return
	}
	old := m.sessionName
	_ = session.Delete(old)
	m.sessionName = name
	m.toast = fmt.Sprintf("renamed session '%s' → '%s'", old, name)
}

// firstUserText is the content of the first message the human actually typed.
func firstUserText(history []api.Message) string {
	for _, msg := range history {
		if isUserTurn(msg) {
			if text := strings.TrimSpace(msg.Content); text != "" {
				return text
			}
		}
	}
	return ""
}

// hasAssistantReply reports whether the conversation already has a completed
// assistant turn — a resumed session's first reply is in the past, so its
// one-shot title generation must not fire again.
func hasAssistantReply(history []api.Message) bool {
	for _, msg := range history {
		if msg.Role == "assistant" && strings.TrimSpace(msg.Content) != "" {
			return true
		}
	}
	return false
}
