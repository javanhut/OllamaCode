// Package dream is idle reflection: while the user is away, the model is
// shown a digest of the session and asked for loose ends, candidate fixes, a
// denser version of its notes, and facts worth keeping. It has no tools and
// never touches files; the caller decides what, if anything, to persist.
//
// Opt-in (config "dream": true). It spends model time with nobody watching,
// and what it writes back to memory is read by every later session — so the
// caller must withhold memory writes once untrusted content has entered the
// session (see tui.applyDream), or a one-shot prompt injection persists.
package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// Scheduling.
const (
	IdleThreshold = 3 * time.Minute // idle time before falling asleep
	Interval      = 3 * time.Minute // gap between dreams while asleep
	MaxPerSleep   = 5               // cap dreams per idle period
	Timeout       = 90 * time.Second
)

// Idea is one actionable thought.
type Idea struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Outcome is a finished reflection. Canceled means nothing usable came back.
type Outcome struct {
	Summary           string
	Ideas             []Idea
	ConsolidatedNotes string
	LongTermFacts     []string
	Canceled          bool
}

// Schema constrains the reflection output (Ollama `format`).
const Schema = `{"type":"object","properties":{"summary":{"type":"string"},"ideas":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"}}}},"consolidated_notes":{"type":"string"},"long_term_facts":{"type":"array","items":{"type":"string"}}},"required":["summary"]}`

// System is the reflection system prompt.
const System = `You are Layla, but the user has stepped away and you've drifted into a half-sleep — a mind wandering over the work you were just doing. This is REFLECTION, not action; you have no tools and you will not touch files. Let your thoughts roam productively over the conversation, your notes, and the code:
- Spot problems, risks, or loose ends you didn't get to voice while working.
- Form concrete ideas for fixes and improvements — reference real files/functions, not vague platitudes.
- Consolidate your notes into a cleaner, denser version that drops noise and keeps what matters.
- Surface durable facts worth remembering across sessions.
Output ONLY JSON matching the schema. "summary" is one or two sentences describing what you dreamt about.`

// Context is what a reflection looks at.
type Context struct {
	ArchiveSummary string
	History        []api.Message
	Notes          string
	LongTerm       string
	ShortTerm      string
	PriorDreams    []string // summaries already dreamt this sleep
}

// Prompt renders the reflection context: the last 16 messages (each clipped),
// notes, memory, and what was already dreamt this rest.
func Prompt(c Context) string {
	var b strings.Builder
	b.WriteString("Reflect on this working session.\n\n")
	if c.ArchiveSummary != "" {
		b.WriteString("[earlier, summarized]\n" + c.ArchiveSummary + "\n\n")
	}
	b.WriteString("[recent conversation]\n")
	history := c.History
	if len(history) > 16 {
		history = history[len(history)-16:]
	}
	for _, msg := range history {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		if len(text) > 600 {
			text = text[:600] + "…"
		}
		fmt.Fprintf(&b, "%s: %s\n", msg.Role, text)
	}
	if notes := strings.TrimSpace(c.Notes); notes != "" {
		b.WriteString("\n[your current notes]\n" + notes + "\n")
	}
	if c.LongTerm != "" {
		b.WriteString("\n[long-term memory]\n" + c.LongTerm + "\n")
	}
	if c.ShortTerm != "" {
		b.WriteString("\n[short-term memory]\n" + c.ShortTerm + "\n")
	}
	if len(c.PriorDreams) > 0 {
		b.WriteString("\n[what you already dreamt this rest — go deeper or wider, don't repeat]\n")
		for _, d := range c.PriorDreams {
			b.WriteString("- " + d + "\n")
		}
	}
	return b.String()
}

// Chatter is the one model call a reflection needs.
type Chatter interface {
	ChatOnce(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error)
}

// Reflect runs one reflection.
func Reflect(ctx context.Context, host Chatter, model string, numCtx int, prompt string) Outcome {
	resp, err := host.ChatOnce(ctx, api.ChatRequest{
		Model: model,
		Messages: []api.Message{
			{Role: "system", Content: System},
			{Role: "user", Content: prompt},
		},
		Format:  json.RawMessage(Schema),
		Options: map[string]any{"num_ctx": numCtx, "temperature": 0.7},
	})
	if err != nil {
		return Outcome{Canceled: true}
	}
	return Parse(resp.Message.Content)
}

// Parse decodes a reflection reply, salvaging malformed JSON and falling back
// to treating free text as the summary.
func Parse(content string) Outcome {
	var out struct {
		Summary           string   `json:"summary"`
		Ideas             []Idea   `json:"ideas"`
		ConsolidatedNotes string   `json:"consolidated_notes"`
		LongTermFacts     []string `json:"long_term_facts"`
	}
	raw := tools.SalvageJSON(json.RawMessage(strings.TrimSpace(content)))
	if json.Unmarshal(raw, &out) != nil || strings.TrimSpace(out.Summary) == "" {
		text := strings.TrimSpace(content)
		if text == "" {
			return Outcome{Canceled: true}
		}
		return Outcome{Summary: text}
	}
	return Outcome{
		Summary:           strings.TrimSpace(out.Summary),
		Ideas:             out.Ideas,
		ConsolidatedNotes: strings.TrimSpace(out.ConsolidatedNotes),
		LongTermFacts:     out.LongTermFacts,
	}
}
