package tui

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// sessionPersist gates all turn-end disk writes (auto-save + checkpoint
// snapshots). It is off until runProgram enables it, so tests and any code that
// drives a bare Model never write session state to the user's real config dir.
var sessionPersist atomic.Bool

func enableSessionPersistence() { sessionPersist.Store(true) }

// stateDir is the per-user ocode state directory (~/.config/ollama_code);
// named sessions live one level down in sessions/.
func stateDir() string {
	return filepath.Dir(session.Dir())
}

// autosavePath is where the last completed turn's session state is written at
// every turn end. It lives outside sessions/ so it never shows up in /sessions.
func autosavePath() string {
	return filepath.Join(stateDir(), "autosave.json")
}

// crashMarkerPath exists while a TUI process is running. A clean quit removes
// it; finding it at startup means the previous run died (crash, kill, power).
func crashMarkerPath() string {
	return filepath.Join(stateDir(), "running.lock")
}

// checkpointPath is the per-workspace on-disk copy of the /undo stack. Keyed by
// workspace so checkpoints from one project can never rewind files in another.
func checkpointPath() string {
	sum := sha1.Sum([]byte(workspaceRoot()))
	return filepath.Join(stateDir(), "checkpoints", hex.EncodeToString(sum[:8])+".json")
}

// autosaveSession persists the session at the end of a completed (or failed)
// turn: history, mode, model, workspace, todos, and notes. Granularity is the
// turn — an interrupted in-flight turn is not saved; crash recovery restores
// the last completed turn. Writes are atomic and best-effort: a failed save
// must never break the turn loop.
func (m *Model) autosaveSession() {
	if !sessionPersist.Load() {
		return
	}
	s := session.Session{
		Name:      "autosave",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Model:     m.modelName,
		Mode:      m.mode.String(),
		Workspace: workspaceRoot(),
		Title:     m.sessionTitle,
		// A generated title stays unpinned; only /title pins. An empty title is
		// fine — SaveTo derives the fallback from the first user message.
		TitlePinned: m.titlePinned,
		Messages:    append([]api.Message(nil), m.history...),
		// The log alone is not the state: without the boundaries a resumed
		// session hands the model back every message compaction already paid to
		// summarize away.
		ArchiveSummary:  m.archiveSummary,
		ArchivedThrough: m.archivedThrough,
		PrunedThrough:   m.prunedThrough,
	}
	if m.notes != nil {
		s.Notes = m.notes.get()
	}
	for _, it := range m.todos.get() {
		s.Todos = append(s.Todos, session.Todo{Content: it.Content, Status: string(it.Status)})
	}
	_ = session.SaveTo(autosavePath(), s)
}

// restoreSession populates m from the auto-save (id == "") or a named session
// written by /save, then reloads the on-disk checkpoint stack so /undo reaches
// across the restart. Nothing to resume is not an error: it leaves m fresh and
// says so in the toast. Built as a method on the already-constructed Model so
// startup stays New() + populate.
func (m *Model) restoreSession(id string) {
	var (
		s   *session.Session
		err error
	)
	if id == "" {
		s, err = session.LoadFrom(autosavePath())
	} else {
		s, err = session.Load(id)
	}
	if err != nil {
		if id == "" {
			m.toast = "nothing to resume — starting fresh"
		} else {
			m.toast = "resume failed: " + err.Error()
		}
		return
	}

	m.history = append([]api.Message(nil), s.Messages...)
	repaired := m.repairInterruptedTurn()
	m.archiveSummary, m.archivedThrough, m.prunedThrough = s.ArchiveSummary, s.ArchivedThrough, s.PrunedThrough
	m.contextSnapshot = nil // the snapshot described the conversation just replaced
	m.sessionName = id
	m.sessionTitle, m.titlePinned = s.Title, s.TitlePinned
	// A resumed conversation's first reply is in the past; the one-shot title
	// generation belongs to the process that was there for it.
	m.titleGenTried = hasAssistantReply(m.history)
	m.turnRecords = nil                             // timings belong to the process that produced them
	m.ratedFrom, m.ratedTo, m.turnRating = 0, 0, "" // and so does the turn a rating would have named
	if m.notes != nil && s.Notes != "" {
		m.notes.set(s.Notes)
	}
	if m.todos != nil && len(s.Todos) > 0 {
		items := make([]todoItem, 0, len(s.Todos))
		for _, t := range s.Todos {
			it := todoItem{Content: t.Content, Status: todoStatus(t.Status)}
			switch it.Status {
			case todoPending, todoInProgress, todoCompleted:
			default:
				it.Status = todoPending
			}
			items = append(items, it)
		}
		m.todos.set(items)
	}
	if mode, ok := parseMode(s.Mode); ok {
		m.mode = mode
	}
	if s.Model != "" {
		// Not persisted to config: resuming borrows the session's model for this
		// process; routing for the restored mode may still override it.
		m.modelName = s.Model
	}
	m.applyRoute(m.mode)
	m.resolveProfile()
	m.loadPersistedCheckpoints()

	toast := fmt.Sprintf("resumed session — %d messages", len(m.history))
	if n := m.todos.openCount(); n > 0 {
		toast += fmt.Sprintf(", %d open todo(s)", n)
	}
	if repaired > 0 {
		toast = "Recovered interrupted turn · " + toast
	}
	if crashedLastRun() {
		toast = "recovered after unclean exit (restored to last completed turn) · " + toast
	}
	if s.Workspace != "" && s.Workspace != workspaceRoot() {
		toast += " · saved in " + s.Workspace
	}
	m.toast = toast
}

// repairInterruptedTurn closes out a turn the previous process died in the
// middle of: the loaded history's tail is an assistant tool-call message whose
// results never landed. For each unanswered call it appends a synthetic error
// result — the same shape dispatch uses for tool failures, so the call/result
// pairing toolResultsAfter and toOpenAIMessages enforce reads as answered —
// then an advisory telling the model the turn was interrupted. Returns how
// many synthetic results were added.
//
// The condition is the imbalance itself, not the crash marker: the marker
// lives for the whole process lifetime, so it can't separate "died mid-turn"
// from "died between turns", and an esc-interrupted batch can leave the same
// imbalance behind on a clean exit. A turn-end save is always balanced, so
// only a genuinely interrupted tail ever triggers this — which also makes it
// idempotent: once repaired, the tail pairs up and a re-resume adds nothing.
// Only the tail is repaired; an imbalance buried behind a later assistant
// message can't be fixed by appending and is left for the send-side pairing
// to drop.
func (m *Model) repairInterruptedTurn() int {
	i := len(m.history) - 1
	answered := 0
	for ; i >= 0; i-- {
		switch m.history[i].Role {
		case "tool":
			answered++
		case "assistant":
			goto tail
		}
	}
	return 0 // no assistant message at all
tail:
	calls := m.history[i].ToolCalls
	if len(calls) == 0 || answered >= len(calls) {
		return 0
	}
	// Results answer calls in order, so the answered prefix of the batch has
	// its results and the missing suffix is calls[answered:].
	for _, call := range calls[answered:] {
		m.history = append(m.history, api.Message{
			Role:     "tool",
			ToolName: call.Function.Name,
			Content:  "error: interrupted by restart — the tool may or may not have completed; verify state before retrying",
		})
	}
	m.history = append(m.history, advisory("[TURN INTERRUPTED — RESTART] The previous turn was cut short by a crash or restart. The tool result(s) above are synthetic: the call(s) may or may not have completed, so verify the current state before retrying or building on them."))
	return len(calls) - answered
}

// markSessionRunning drops the crash marker. Called once the TUI is up.
func markSessionRunning() {
	if !sessionPersist.Load() {
		return
	}
	path := crashMarkerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, fmt.Appendf(nil, "pid %d started %s\n", os.Getpid(), time.Now().Format(time.RFC3339)), 0o644)
}

// clearSessionMarker removes the crash marker on a clean exit.
func clearSessionMarker() {
	_ = os.Remove(crashMarkerPath())
}

// crashedLastRun reports whether the previous TUI process exited without
// clearing its marker.
func crashedLastRun() bool {
	_, err := os.Stat(crashMarkerPath())
	return err == nil
}

// writeFileAtomic writes data to path via a temp file + rename in the same
// directory, so readers never see a torn write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".atomic-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
