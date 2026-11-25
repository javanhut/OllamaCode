package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/internal/dream"
)

// Dream-mode tuning lives in internal/dream.
const (
	dreamIdleThreshold = dream.IdleThreshold
	dreamInterval      = dream.Interval
	maxDreamsPerSleep  = dream.MaxPerSleep
)

type dreamIdea = dream.Idea

type dreamRecord struct {
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

// dreamsOn reports whether dream mode is enabled. Opt-in ("dream": true):
// it spends model time with nobody watching and writes to long-term memory.
func (m *Model) dreamsOn() bool {
	return m.cfg.Dream != nil && *m.cfg.Dream
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
	// Dreaming is unprompted and fires while the user is away. Never run it on a
	// routed provider — that spends a metered API with nobody watching. The
	// default host is the one they already opted into.
	if m.host.URL() != m.cfg.Host {
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
// reflection (internal/dream).
func (m *Model) dreamCmd() tea.Cmd {
	host := m.host
	model := m.modelName
	numCtx := m.contextLimit
	prompt := dream.Prompt(m.dreamContext())

	ctx, cancel := context.WithTimeout(context.Background(), dream.Timeout)
	m.dreamCancel = cancel

	return func() tea.Msg {
		out := dream.Reflect(ctx, host, model, numCtx, prompt)
		return dreamDoneMsg{
			summary: out.Summary, ideas: out.Ideas, consolidatedNotes: out.ConsolidatedNotes,
			longTermFacts: out.LongTermFacts, canceled: out.Canceled,
		}
	}
}

// dreamContext gathers what a reflection looks at.
func (m *Model) dreamContext() dream.Context {
	c := dream.Context{ArchiveSummary: m.archiveSummary, History: m.history}
	if m.notes != nil {
		c.Notes = m.notes.get()
	}
	if m.memory != nil {
		c.LongTerm, c.ShortTerm = m.memory.LongTermSummary(), m.memory.ShortTermSummary()
	}
	for _, d := range m.pendingDreams {
		c.PriorDreams = append(c.PriorDreams, d.summary)
	}
	return c
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

	d := dreamRecord{at: time.Now(), summary: msg.summary, ideas: msg.ideas}
	m.pendingDreams = append(m.pendingDreams, d)
	m.dreams = append(m.dreams, d)

	// Auto-consolidate: promote short-term memory to long-term, persist durable
	// facts, and replace notes with the denser version — backing up the originals
	// so nothing is lost.
	// Not after untrusted content: a dream reflects on the whole conversation,
	// and a fact distilled from an injected web page or MCP result would be
	// written to long-term memory and obeyed in every future session —
	// persistence for a one-shot injection. The ideas still surface; only the
	// automatic writes are withheld.
	if m.sessionTainted {
		m.toast = "dreamt — memory not auto-updated (untrusted content was read this session)"
		return
	}
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
