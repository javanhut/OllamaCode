package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// Forking and rewinding both fall out of the log being append-only: a message's
// position in m.history is fixed for the life of the session, so "the
// conversation as it stood at turn N" is m.history[:i] plus what stateAt does
// to the two boundaries. Neither needed a new data structure, and neither could have
// been written while compaction was still slicing the front off history —
// every index would have meant something different after every pass.

// userTurnStarts returns the log indexes of the user's own messages, oldest
// first. Advisories ride the user role, so isUserTurn is what separates the
// human's turns from the harness talking to the model.
func (m *Model) userTurnStarts() []int {
	var starts []int
	for i, msg := range m.history {
		if isUserTurn(msg) {
			starts = append(starts, i)
		}
	}
	return starts
}

// turnCut maps "n user turns back" to a log index: 1 is the start of the most
// recent turn, 2 the one before it. n of 0 means "right here", the whole log.
func (m *Model) turnCut(n int) (int, error) {
	starts := m.userTurnStarts()
	if n <= 0 {
		return len(m.history), nil
	}
	if n > len(starts) {
		return 0, fmt.Errorf("only %d turns in this conversation", len(starts))
	}
	return starts[len(starts)-n], nil
}

// stateAt is the conversation as it was at log index cut.
//
// A cut at or before a boundary retires that boundary rather than clamping it
// to the cut: everything that survives is then in front of it, so clamping
// would archive the whole conversation behind a summary of messages that are
// no longer there — the model would see nothing at all. The archive summary
// goes with its boundary for the same reason, and the prune boundary resets so
// a short conversation is not left with every tool result stubbed.
func (m *Model) stateAt(cut int) (msgs []api.Message, summary string, archived, pruned int) {
	cut = min(max(cut, 0), len(m.history))
	msgs = append([]api.Message(nil), m.history[:cut]...)

	archived, summary = m.archivedThrough, m.archiveSummary
	if cut <= m.archivedThrough {
		archived, summary = 0, ""
	}
	pruned = m.prunedThrough
	if cut <= m.prunedThrough {
		pruned = 0
	}
	return msgs, summary, archived, pruned
}

// forkSession writes the conversation as of cut to a named session, leaving the
// live one alone. Notes and todos come along because a plan in notes is the
// state a branch is usually taken to preserve.
func (m *Model) forkSession(cut int, name string) error {
	msgs, summary, archived, pruned := m.stateAt(cut)
	s := session.Session{
		Name:            name,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		Model:           m.modelName,
		Mode:            m.mode.String(),
		Workspace:       workspaceRoot(),
		Messages:        msgs,
		ArchiveSummary:  summary,
		ArchivedThrough: archived,
		PrunedThrough:   pruned,
	}
	if m.notes != nil {
		s.Notes = m.notes.get()
	}
	if m.todos != nil {
		for _, it := range m.todos.get() {
			s.Todos = append(s.Todos, session.Todo{Content: it.Content, Status: string(it.Status)})
		}
	}
	return session.Save(s)
}

// forkCommand: /fork [n] [name] — save the conversation as it stood n user
// turns back (default: as it stands now) under name, and keep going here.
// Load the branch with /load when you want it.
func (m *Model) forkCommand(args string) {
	n, name := parseTurnArg(args)
	cut, err := m.turnCut(n)
	if err != nil {
		m.toast = err.Error()
		return
	}
	if name == "" {
		name = "fork_" + time.Now().Format("2006-01-02_15-04-05")
	}
	if err := m.forkSession(cut, name); err != nil {
		m.toast = "fork failed: " + err.Error()
		return
	}
	where := "here"
	if n > 0 {
		where = fmt.Sprintf("%d turn(s) back", n)
	}
	m.toast = fmt.Sprintf("forked %s into '%s' — /load %s to continue it", where, name, name)
}

// rewindCommand: /rewind [n] — drop the last n user turns (default 1) and
// continue from there. The discarded tail is forked to a session first, so a
// rewind is never a one-way door.
//
// Conversation only: files stay as they are. Reverting those is /undo, which
// pops one checkpoint at a time and knows what it wrote; guessing at the
// intersection of "n turns of messages" and "which file writes to unwind" is
// how an undo eats work it shouldn't.
func (m *Model) rewindCommand(args string) {
	n := 1
	if trimmed := strings.TrimSpace(args); trimmed != "" {
		parsed, err := strconv.Atoi(trimmed)
		if err != nil || parsed < 1 {
			m.toast = "usage: /rewind [turns] — how many of your turns to drop (default 1)"
			return
		}
		n = parsed
	}
	cut, err := m.turnCut(n)
	if err != nil {
		m.toast = err.Error()
		return
	}
	dropped := len(m.history) - cut
	if dropped == 0 {
		m.toast = "nothing to rewind"
		return
	}

	backup := "rewind_" + time.Now().Format("2006-01-02_15-04-05")
	saved := m.forkSession(len(m.history), backup) == nil

	m.rewindTo(cut)

	msg := fmt.Sprintf("rewound %d turn(s) — %d messages dropped, files untouched", n, dropped)
	if saved {
		msg += " (saved as '" + backup + "')"
	}
	m.toast = msg
}

// rewindTo truncates the log at cut and resets everything derived from the part
// that is now gone. The one place that shortens m.history, and it does the
// whole reset at once precisely so no reader is left holding an index into
// messages that no longer exist.
func (m *Model) rewindTo(cut int) {
	if m.stream != nil && m.stream.cancel != nil {
		m.stream.cancel()
	}
	m.turnGen++ // orphan anything in flight from the turn being dropped
	m.streaming = false
	m.stream = nil
	m.pending = nil
	m.streamBuf.Reset()
	m.streamThinking.Reset()
	if m.state == statePermission {
		m.state = stateChat
	}

	msgs, summary, archived, pruned := m.stateAt(cut)
	m.history = msgs
	m.archiveSummary, m.archivedThrough, m.prunedThrough = summary, archived, pruned

	for idx := range m.turnRecords {
		if idx >= len(m.history) {
			delete(m.turnRecords, idx)
		}
	}
	// Every measurement describes a prompt that no longer exists.
	m.totalTokens, m.lastPromptEval, m.prevPromptEval = 0, 0, 0
	m.resetTurnGuards() // also re-anchors the turn clock off the shortened log

	m.refreshTranscript()
	m.viewport.GotoBottom()
}

// parseTurnArg reads "[n] [name]": a leading integer is the turn count, and
// anything else is the whole name. A name that happens to start with a digit
// stays a name — "/fork 2" is a turn, "/fork 2nd-attempt" is not.
func parseTurnArg(args string) (int, string) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return 0, ""
	}
	if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
		return n, strings.Join(fields[1:], " ")
	}
	return 0, strings.Join(fields, " ")
}

// saveCommand: /save [name] — the whole conversation under a name, which is a
// fork taken at the present moment. One session writer, so a field added to
// what a session carries cannot reach one command and miss the other.
func (m *Model) saveCommand(name string) {
	if name == "" {
		name = time.Now().Format("2006-01-02_15-04-05")
	}
	if err := m.forkSession(len(m.history), name); err != nil {
		m.toast = "save failed: " + err.Error()
		return
	}
	m.toast = "saved session '" + name + "'"
}

// loadCommand: /load <name> — replace the live conversation with a saved one.
func (m *Model) loadCommand(name string) {
	if name == "" {
		m.toast = "usage: /load <name>"
		return
	}
	s, err := session.Load(name)
	if err != nil {
		m.toast = "load failed: " + err.Error()
		return
	}
	m.history = append([]api.Message(nil), s.Messages...)
	m.archiveSummary, m.archivedThrough, m.prunedThrough = s.ArchiveSummary, s.ArchivedThrough, s.PrunedThrough
	m.turnRecords = nil // timings belong to the session we just left
	m.notes.set(s.Notes)
	m.modelName = s.Model
	if s.Mode != "" {
		switch s.Mode {
		case "explore":
			m.mode = ExploreMode
		case "plan":
			m.mode = PlanMode
		case "write":
			m.mode = WriteMode
		case "auto":
			m.mode = AutoMode
		}
	}
	m.cfg.Model = m.modelName
	saveConfig(m.cfg)
	// Save the session's model as the default first: routing may re-point the
	// active model for the restored mode, and that must not persist.
	m.applyRoute(m.mode)
	m.resolveProfile()
	m.refreshTranscript()
	m.viewport.GotoBottom()
	m.toast = "loaded session '" + name + "'"
}
