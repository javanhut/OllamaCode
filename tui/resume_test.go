package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// withSessionState redirects the session store (and with it the auto-save,
// crash marker, and checkpoint files, which all derive from session.Dir) into
// a temp dir and enables turn-end persistence. Not parallel-safe: both knobs
// are package globals.
func withSessionState(t *testing.T) {
	t.Helper()
	restore := session.SetDirForTesting(filepath.Join(t.TempDir(), "sessions"))
	sessionPersist.Store(true)
	t.Cleanup(func() {
		sessionPersist.Store(false)
		restore()
	})
}

// TestAutosaveRoundTrip: the state written at turn end loads back with
// history, mode, model, todos, and notes intact.
func TestAutosaveRoundTrip(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir()) // sessionNotes writes .ollama_notes.md to cwd

	m := &Model{
		notes:     &sessionNotes{},
		todos:     &todoList{},
		mode:      PlanMode,
		modelName: "testmodel",
		history: []api.Message{
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: "answer"},
		},
	}
	m.notes.set("the plan")
	m.todos.set([]todoItem{
		{Content: "done step", Status: todoCompleted},
		{Content: "open step", Status: todoInProgress},
	})
	m.autosaveSession()

	s, err := session.LoadFrom(autosavePath())
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "testmodel" || s.Mode != "plan" || s.Notes != "the plan" {
		t.Fatalf("scalars drifted: %+v", s)
	}
	if len(s.Messages) != 2 || s.Messages[0].Content != "question" {
		t.Fatalf("history drifted: %+v", s.Messages)
	}
	if len(s.Todos) != 2 || s.Todos[1].Status != "in_progress" {
		t.Fatalf("todos drifted: %+v", s.Todos)
	}
	if s.Workspace == "" {
		t.Fatal("workspace not recorded")
	}
}

// TestAutosaveGatedOff: a bare Model (tests, headless) must not write session
// state; persistence turns on only under runProgram.
func TestAutosaveGatedOff(t *testing.T) {
	restore := session.SetDirForTesting(filepath.Join(t.TempDir(), "sessions"))
	defer restore()
	m := &Model{todos: &todoList{}}
	m.autosaveSession()
	if _, err := os.Stat(autosavePath()); !os.IsNotExist(err) {
		t.Fatal("autosave wrote with persistence disabled")
	}
}

// TestResumeRestoresState: a fresh Model populated by restoreSession carries
// the saved history, todos, notes, mode, and model.
func TestResumeRestoresState(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())

	saved := session.Session{
		Name:  "autosave",
		Model: "testmodel",
		Mode:  "write",
		Notes: "saved notes",
		Todos: []session.Todo{
			{Content: "finished", Status: "completed"},
			{Content: "pending", Status: "pending"},
			{Content: "garbage", Status: "bogus"}, // unknown statuses normalize to pending
		},
		Messages: []api.Message{{Role: "user", Content: "old question"}},
	}
	if err := session.SaveTo(autosavePath(), saved); err != nil {
		t.Fatal(err)
	}

	m := &Model{
		notes: &sessionNotes{},
		todos: &todoList{},
		cfg: config{Profiles: map[string]ModelProfile{
			// Cached profile: resolveProfile must not hit the network in a test.
			"testmodel": {NumCtx: 4096, ParamsB: 7},
		}},
	}
	m.restoreSession("")

	if len(m.history) != 1 || m.history[0].Content != "old question" {
		t.Fatalf("history not restored: %+v", m.history)
	}
	if m.mode != WriteMode {
		t.Fatalf("mode = %v, want write", m.mode)
	}
	if m.modelName != "testmodel" {
		t.Fatalf("model = %q", m.modelName)
	}
	if m.notes.get() != "saved notes" {
		t.Fatalf("notes = %q", m.notes.get())
	}
	items := m.todos.get()
	if len(items) != 3 || items[2].Status != todoPending {
		t.Fatalf("todos = %+v", items)
	}
	if open := m.todos.openCount(); open != 2 {
		t.Fatalf("open todos = %d, want 2", open)
	}
	if !strings.Contains(m.toast, "resumed session") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// TestResumeNothingToResume: --resume with no auto-save starts fresh and says
// so instead of failing.
func TestResumeNothingToResume(t *testing.T) {
	withSessionState(t)
	m := &Model{todos: &todoList{}}
	m.restoreSession("")
	if len(m.history) != 0 {
		t.Fatal("history should be empty")
	}
	if !strings.Contains(m.toast, "nothing to resume") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// TestResumeNamedSession: --resume <name> loads a session written by /save.
func TestResumeNamedSession(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	if err := session.Save(session.Session{
		Name:     "named",
		Model:    "m1",
		Messages: []api.Message{{Role: "user", Content: "named q"}},
	}); err != nil {
		t.Fatal(err)
	}
	m := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m.restoreSession("named")
	if len(m.history) != 1 || m.history[0].Content != "named q" {
		t.Fatalf("history = %+v", m.history)
	}
}

// TestCrashMarkerDirtyDetection: the marker exists while running, a clean exit
// clears it, and a leftover marker reads as a crash.
func TestCrashMarkerDirtyDetection(t *testing.T) {
	withSessionState(t)
	if crashedLastRun() {
		t.Fatal("no marker should exist yet")
	}
	markSessionRunning()
	if !crashedLastRun() {
		t.Fatal("marker written but not detected")
	}
	clearSessionMarker()
	if crashedLastRun() {
		t.Fatal("marker cleared but still detected")
	}
}

// TestCrashRecoveryToast: resuming with a dirty marker announces the recovery
// and its granularity (last completed turn).
func TestCrashRecoveryToast(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	if err := session.SaveTo(autosavePath(), session.Session{
		Messages: []api.Message{{Role: "user", Content: "q"}},
	}); err != nil {
		t.Fatal(err)
	}
	markSessionRunning() // simulate the previous run dying mid-flight
	m := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m.restoreSession("")
	if !strings.Contains(m.toast, "recovered after unclean exit") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// TestCheckpointPersistsAcrossReopen: /undo must work in a later process —
// snapshots written by one Model restore files after a fresh Model reloads
// the on-disk stack, and the undo pop is synced back to disk.
func TestCheckpointPersistsAcrossReopen(t *testing.T) {
	withSessionState(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "Process 1": snapshot, mutate, end the turn.
	m1 := &Model{todos: &todoList{}}
	m1.snapshotBeforeMutate([]string{f})
	if err := os.WriteFile(f, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	m1.finalizeCheckpoint("turn 1")
	if _, err := os.Stat(checkpointPath()); err != nil {
		t.Fatalf("checkpoint file not written: %v", err)
	}

	// "Process 2": reload from disk, undo.
	m2 := &Model{todos: &todoList{}}
	m2.loadPersistedCheckpoints()
	summary, touched := m2.undoLast()
	if len(touched) != 1 {
		t.Fatalf("undo touched %v", touched)
	}
	if got, _ := os.ReadFile(f); string(got) != "original" {
		t.Fatalf("undo across reopen restored %q; summary: %s", got, summary)
	}

	// The pop synced to disk: a third reload finds an empty stack.
	m3 := &Model{todos: &todoList{}}
	m3.loadPersistedCheckpoints()
	if msg, _ := m3.undoLast(); msg != "nothing to undo" {
		t.Fatalf("expected empty stack after synced pop, got %q", msg)
	}
}

// TestCheckpointPruningOnDisk: the on-disk stack honors maxUndoDepth, dropping
// the oldest checkpoints exactly like the in-memory one.
func TestCheckpointPruningOnDisk(t *testing.T) {
	withSessionState(t)
	dir := t.TempDir()
	m := &Model{todos: &todoList{}}
	turns := maxUndoDepth + 5
	for i := range turns {
		f := filepath.Join(dir, string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		m.snapshotBeforeMutate([]string{f})
		m.finalizeCheckpoint(fmt.Sprintf("turn-%02d", i))
	}

	data, err := os.ReadFile(checkpointPath())
	if err != nil {
		t.Fatal(err)
	}
	var persisted []persistedTurn
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != maxUndoDepth {
		t.Fatalf("disk stack = %d checkpoints, want %d", len(persisted), maxUndoDepth)
	}
	// The oldest kept checkpoint is the newest-surviving turn: turns 00-04 are
	// pruned, turn-05 is the first entry.
	if persisted[0].Label != "turn-05" {
		t.Fatalf("first kept checkpoint = %q, want turn-05", persisted[0].Label)
	}

	m2 := &Model{todos: &todoList{}}
	m2.loadPersistedCheckpoints()
	m2.ckpt.mu.Lock()
	got := len(m2.ckpt.stack)
	m2.ckpt.mu.Unlock()
	if got != maxUndoDepth {
		t.Fatalf("reloaded stack = %d, want %d", got, maxUndoDepth)
	}
}
