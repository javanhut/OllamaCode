package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

// Dream-mode tuning.
const (
	dreamIdleThreshold = 3 * time.Minute // idle time before falling asleep
	dreamInterval      = 3 * time.Minute // gap between dreams while asleep
	maxDreamsPerSleep  = 5               // cap dreams per idle period
)

type dreamIdea struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type dream struct {
	at      time.Time
	summary string
	ideas   []dreamIdea
}

type dreamDoneMsg struct {
	summary           string
	ideas             []dreamIdea
	consolidatedNotes string
	longTermFacts     []string
	canceled          bool
}

// dreamSchema constrains the reflection output (Ollama `format`).
const dreamSchema = `{"type":"object","properties":{"summary":{"type":"string"},"ideas":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"}}}},"consolidated_notes":{"type":"string"},"long_term_facts":{"type":"array","items":{"type":"string"}}},"required":["summary"]}`

const dreamSystem = `You are Layla, but the user has stepped away and you've drifted into a half-sleep — a mind wandering over the work you were just doing. This is REFLECTION, not action; you have no tools and you will not touch files. Let your thoughts roam productively over the conversation, your notes, and the code:
- Spot problems, risks, or loose ends you didn't get to voice while working.
- Form concrete ideas for fixes and improvements — reference real files/functions, not vague platitudes.
- Consolidate your notes into a cleaner, denser version that drops noise and keeps what matters.
- Surface durable facts worth remembering across sessions.
Output ONLY JSON matching the schema. "summary" is one or two sentences describing what you dreamt about.`

// dreamsOn reports whether dream mode is enabled (default true).
func (m *Model) dreamsOn() bool {
	return m.cfg.Dream == nil || *m.cfg.Dream
}

// maybeDream is invoked on each tick. It returns a command to start a dream when
// the session has been idle past the threshold (and we haven't hit the per-sleep
// cap or the inter-dream interval), or nil.
func (m *Model) maybeDream() tea.Cmd {
	if !m.dreamsOn() || m.streaming || m.pending != nil || m.dreaming {
		return nil
	}
	if m.state != stateChat || m.modelName == "" || len(m.history) < 2 {
		return nil
	}
	if time.Since(m.lastActivity) < dreamIdleThreshold {
		return nil
	}
	m.asleep = true
	if m.dreamCount >= maxDreamsPerSleep {
		return nil
	}
	if !m.lastDreamAt.IsZero() && time.Since(m.lastDreamAt) < dreamInterval {
		return nil
	}
	m.dreaming = true
	return m.dreamCmd()
}

// dreamCmd snapshots the current context in the UI loop, then runs a background
// reflection via constrained-decoding ChatOnce.
func (m *Model) dreamCmd() tea.Cmd {
	host := m.host
	model := m.modelName
	numCtx := m.contextLimit
	prompt := m.buildDreamPrompt()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	m.dreamCancel = cancel

	return func() tea.Msg {
		resp, err := host.ChatOnce(ctx, api.ChatRequest{
			Model: model,
			Messages: []api.Message{
				{Role: "system", Content: dreamSystem},
				{Role: "user", Content: prompt},
			},
			Format:  json.RawMessage(dreamSchema),
			Options: map[string]any{"num_ctx": numCtx, "temperature": 0.7},
		})
		if err != nil {
			return dreamDoneMsg{canceled: true}
		}
		var out struct {
			Summary           string      `json:"summary"`
			Ideas             []dreamIdea `json:"ideas"`
			ConsolidatedNotes string      `json:"consolidated_notes"`
			LongTermFacts     []string    `json:"long_term_facts"`
		}
		raw := salvageJSON(json.RawMessage(strings.TrimSpace(resp.Message.Content)))
		if json.Unmarshal(raw, &out) != nil || strings.TrimSpace(out.Summary) == "" {
			// Fall back: treat the whole reply as a freeform dream summary.
			text := strings.TrimSpace(resp.Message.Content)
			if text == "" {
				return dreamDoneMsg{canceled: true}
			}
			return dreamDoneMsg{summary: text}
		}
		return dreamDoneMsg{
			summary:           strings.TrimSpace(out.Summary),
			ideas:             out.Ideas,
			consolidatedNotes: strings.TrimSpace(out.ConsolidatedNotes),
			longTermFacts:     out.LongTermFacts,
		}
	}
}

// buildDreamPrompt assembles the reflection context from recent history, notes,
// memory, and prior dreams this sleep.
func (m *Model) buildDreamPrompt() string {
	var b strings.Builder
	b.WriteString("Reflect on this working session.\n\n")
	if m.archiveSummary != "" {
		b.WriteString("[earlier, summarized]\n" + m.archiveSummary + "\n\n")
	}
	b.WriteString("[recent conversation]\n")
	start := 0
	if len(m.history) > 16 {
		start = len(m.history) - 16
	}
	for _, msg := range m.history[start:] {
		c := strings.TrimSpace(msg.Content)
		if c == "" {
			continue
		}
		if len(c) > 600 {
			c = c[:600] + "…"
		}
		fmt.Fprintf(&b, "%s: %s\n", msg.Role, c)
	}
	if notes := strings.TrimSpace(m.notes.get()); notes != "" {
		b.WriteString("\n[your current notes]\n" + notes + "\n")
	}
	if m.memory != nil {
		if lt := m.memory.LongTermSummary(); lt != "" {
			b.WriteString("\n[long-term memory]\n" + lt + "\n")
		}
		if st := m.memory.ShortTermSummary(); st != "" {
			b.WriteString("\n[short-term memory]\n" + st + "\n")
		}
	}
	if len(m.pendingDreams) > 0 {
		b.WriteString("\n[what you already dreamt this rest — go deeper or wider, don't repeat]\n")
		for _, d := range m.pendingDreams {
			b.WriteString("- " + d.summary + "\n")
		}
	}
	return b.String()
}

// applyDream stores a completed dream and (per the user's opt-in) auto-
// consolidates memory and notes. Runs in the UI loop.
func (m *Model) applyDream(msg dreamDoneMsg) {
	m.dreaming = false
	m.dreamCancel = nil
	if msg.canceled {
		return
	}
	m.lastDreamAt = time.Now()
	m.dreamCount++

	d := dream{at: time.Now(), summary: msg.summary, ideas: msg.ideas}
	m.pendingDreams = append(m.pendingDreams, d)
	m.dreams = append(m.dreams, d)

	// Auto-consolidate: promote short-term memory to long-term, persist durable
	// facts, and replace notes with the denser version — backing up the originals
	// so nothing is lost.
	if m.memory != nil {
		_, _ = m.memory.PromoteAll()
		for _, f := range msg.longTermFacts {
			if strings.TrimSpace(f) != "" {
				_, _ = m.memory.Remember(strings.TrimSpace(f), true)
			}
		}
	}
	if nc := strings.TrimSpace(msg.consolidatedNotes); len(nc) > 20 {
		old := m.notes.get()
		if nc != strings.TrimSpace(old) {
			if m.kvStore != nil {
				_ = m.kvStore.Set("notes_predream_backup", old)
			}
			m.notesBackup = old
			m.notes.set(nc)
			m.notesViewport.SetContent(m.renderNotesMarkdown(nc, m.notesViewport.Width()))
		}
	}
	m.toast = fmt.Sprintf("dreamt (%d/%d) — %s", m.dreamCount, maxDreamsPerSleep, truncatePlain(d.summary, 44))
}

// wake exits dream mode, cancelling any in-flight dream. Dreams are surfaced at
// the next submit so the live model can mention them.
func (m *Model) wake() {
	if m.dreamCancel != nil {
		m.dreamCancel()
		m.dreamCancel = nil
	}
	m.asleep = false
	m.dreaming = false
	m.dreamCount = 0
	m.lastDreamAt = time.Time{}
}

// dreamWakeContext returns a system message describing the dreams gathered this
// sleep (to inject before the model's first reply), then clears the pending set.
func (m *Model) dreamWakeContext() (string, bool) {
	if len(m.pendingDreams) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("[DREAMS WHILE THE USER WAS AWAY] You drifted into reflection while idle and had these thoughts. Mention briefly and naturally that you had some ideas while they were gone, then offer the genuinely useful ones — don't dump all of it verbatim.\n")
	for i, d := range m.pendingDreams {
		fmt.Fprintf(&b, "\nDream %d: %s\n", i+1, d.summary)
		for _, idea := range d.ideas {
			fmt.Fprintf(&b, "  - %s: %s\n", idea.Title, idea.Detail)
		}
	}
	if m.notesBackup != "" {
		b.WriteString("\n(While dreaming you also consolidated your notes and promoted memory. The previous notes are backed up — the user can run /notes restore.)\n")
	}
	m.pendingDreams = nil
	return b.String(), true
}

// dreamLog renders the full session dream log for /dreams.
func (m *Model) dreamLog() string {
	if len(m.dreams) == 0 {
		return "No dreams yet — I drift off and reflect after 3 minutes idle (toggle with /dream)."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Dream log (%d this session):\n", len(m.dreams))
	for i, d := range m.dreams {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, d.summary)
		for _, idea := range d.ideas {
			fmt.Fprintf(&b, "   - %s: %s\n", idea.Title, idea.Detail)
		}
	}
	return b.String()
}
