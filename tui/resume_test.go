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
	"github.com/javanhut/ollama_code/tools"
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

// TestResumeRepairsInterruptedTurn: a crash mid-batch leaves the saved tail
// with tool calls whose results never landed; resume closes them out with
// synthetic error results and an advisory, and says so once.
func TestResumeRepairsInterruptedTurn(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	if err := session.SaveTo(autosavePath(), session.Session{
		Messages: []api.Message{
			{Role: "user", Content: "fix the build"},
			{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`), tc("run_shell", `{"command":"go build ./..."}`)}},
			{Role: "tool", ToolName: "read_file", Content: "contents"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	markSessionRunning() // simulate the previous run dying mid-flight

	m := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m.restoreSession("")

	if len(m.history) != 5 {
		t.Fatalf("history = %d messages, want 3 loaded + 1 synthetic result + 1 advisory", len(m.history))
	}
	synthetic := m.history[3]
	if synthetic.Role != "tool" || synthetic.ToolName != "run_shell" {
		t.Fatalf("synthetic result = %+v, want a tool result named after the unanswered call", synthetic)
	}
	if !strings.Contains(synthetic.Content, "error: interrupted by restart") {
		t.Fatalf("synthetic content = %q", synthetic.Content)
	}
	last := m.history[4]
	if !last.Advisory || !strings.Contains(last.Content, "[TURN INTERRUPTED") {
		t.Fatalf("closing note = %+v, want an advisory marking the interruption", last)
	}
	if !strings.Contains(m.toast, "Recovered interrupted turn") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// TestResumeLeavesBalancedHistoryUntouched: a cleanly ended tail — a complete
// tool result or plain assistant text — gets no synthetic additions, even with
// a dirty crash marker (the process can die between turns too).
func TestResumeLeavesBalancedHistoryUntouched(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	balanced := []api.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}},
		{Role: "tool", ToolName: "read_file", Content: "contents"},
		{Role: "assistant", Content: "here is what a.go does"},
	}
	if err := session.SaveTo(autosavePath(), session.Session{Messages: balanced}); err != nil {
		t.Fatal(err)
	}
	markSessionRunning()

	m := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m.restoreSession("")
	if len(m.history) != len(balanced) {
		t.Fatalf("balanced history grew to %d messages", len(m.history))
	}
	if strings.Contains(m.toast, "Recovered interrupted turn") {
		t.Fatalf("nothing was repaired but toast = %q", m.toast)
	}
}

// TestRepairIdempotentAcrossResumes: the repair persists with the next save
// and re-resuming the repaired session adds nothing — the synthetic results
// themselves make the tail balanced.
func TestRepairIdempotentAcrossResumes(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	if err := session.SaveTo(autosavePath(), session.Session{
		Messages: []api.Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m1 := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m1.restoreSession("")
	repaired := len(m1.history)
	if repaired != 4 { // 2 loaded + synthetic result + advisory
		t.Fatalf("first resume left %d messages, want 4", repaired)
	}
	if n := m1.repairInterruptedTurn(); n != 0 {
		t.Fatalf("second repair added %d results to an already balanced tail", n)
	}
	m1.autosaveSession() // persist the repair, as the next turn end would

	m2 := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m2.restoreSession("")
	if len(m2.history) != repaired {
		t.Fatalf("re-resume grew history to %d, want the repaired %d", len(m2.history), repaired)
	}
	if strings.Contains(m2.toast, "Recovered interrupted turn") {
		t.Fatalf("re-resume of a repaired session toasted: %q", m2.toast)
	}
}

// TestRepairAnswersEveryUnansweredCall: a batch that died before any result
// landed gets one synthetic result per call, named in call order so the
// positional pairing attributes each to the right call.
func TestRepairAnswersEveryUnansweredCall(t *testing.T) {
	m := &Model{history: []api.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []tools.ToolCall{
			tc("read_file", `{"path":"a.go"}`),
			tc("grep", `{"pattern":"x"}`),
			tc("run_shell", `{"command":"go test ./..."}`),
		}},
	}}
	if n := m.repairInterruptedTurn(); n != 3 {
		t.Fatalf("repaired %d calls, want 3", n)
	}
	wantNames := []string{"read_file", "grep", "run_shell"}
	for i, name := range wantNames {
		got := m.history[2+i]
		if got.Role != "tool" || got.ToolName != name {
			t.Fatalf("synthetic %d = %+v, want tool result for %s", i, got, name)
		}
	}
	if last := m.history[len(m.history)-1]; !last.Advisory {
		t.Fatalf("last message = %+v, want the advisory", last)
	}
	// Older history is untouched: only the tail was appended to.
	if m.history[0].Content != "q" || len(m.history[1].ToolCalls) != 3 {
		t.Fatalf("loaded history was modified: %+v", m.history[:2])
	}
}

// TestRepairedTailSurvivesAssembly: the synthetic shape passes the same
// pairing rules the send path enforces — windowing never orphans a tool
// result, and every assistant tool-call in the assembled request is followed
// by enough tool results to answer it.
func TestRepairedTailSurvivesAssembly(t *testing.T) {
	big := strings.Repeat("z", 200)
	m := &Model{host: api.OllamaHost{}, notes: &sessionNotes{}, contextLimit: 4096}
	m.history = []api.Message{
		{Role: "user", Content: big},
		{Role: "assistant", Content: big},
		{Role: "user", Content: "fix the build"},
		{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`), tc("run_shell", `{"command":"go build ./..."}`)}},
		{Role: "tool", ToolName: "read_file", Content: big},
	}
	if n := m.repairInterruptedTurn(); n != 1 {
		t.Fatalf("repaired %d calls, want 1", n)
	}

	out := m.assembleMessages("")
	// The same accounting toOpenAIMessages applies: a tool message with no
	// outstanding call is an orphan, and a call left outstanding at the end is
	// unanswered. Both get the whole request rejected by strict providers.
	outstanding := 0
	for i, msg := range out {
		switch msg.Role {
		case "assistant":
			outstanding += len(msg.ToolCalls)
		case "tool":
			if outstanding == 0 {
				t.Fatalf("assembled request has an orphaned tool result at %d", i)
			}
			outstanding--
		}
	}
	if outstanding != 0 {
		t.Fatalf("assembled request ends with %d unanswered tool call(s)", outstanding)
	}
}

// TestLoadRepairsInterruptedTurn: /load gets the same repair as --resume.
func TestLoadRepairsInterruptedTurn(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)
	if err := session.Save(session.Session{
		Name: "crashed",
		Messages: []api.Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", ToolCalls: []tools.ToolCall{tc("read_file", `{"path":"a.go"}`)}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m.loadCommand("crashed")
	if len(m.history) != 4 { // 2 loaded + synthetic result + advisory
		t.Fatalf("history = %d messages, want 4", len(m.history))
	}
	if got := m.history[2]; got.Role != "tool" || !strings.Contains(got.Content, "error: interrupted by restart") {
		t.Fatalf("synthetic result = %+v", got)
	}
	if !strings.Contains(m.toast, "Recovered interrupted turn") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// TestCheckpointPersistsAcrossReopen: /undo must work in a later process —
// snapshots written by one Model restore files after a fresh Model reloads
// the on-disk stack, and the undo pop is synced back to disk.
func TestCheckpointPersistsAcrossReopen(t *testing.T) {
	withSessionState(t)
	dir := ckptWorkspace(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "Process 1": snapshot, mutate, end the turn.
	m1 := &Model{todos: &todoList{}}
	m1.snapshotBeforeMutate()
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

	// A file left by the pre-git format carries inline contents and no tree id.
	// Replaying one as a revert would reset the workspace to an empty tree, so
	// those entries are dropped instead of trusted.
	if err := os.WriteFile(checkpointPath(), []byte(`[{"label":"old","snaps":{"a.txt":{"existed":true}}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := &Model{todos: &todoList{}}
	legacy.loadPersistedCheckpoints()
	if msg, touched := legacy.undoLast(); touched != nil || msg != "nothing to undo" {
		t.Fatalf("legacy checkpoint file was replayed: %q %v", msg, touched)
	}
}

// TestCheckpointPruningOnDisk: the on-disk stack honors maxUndoDepth, dropping
// the oldest checkpoints exactly like the in-memory one.
func TestCheckpointPruningOnDisk(t *testing.T) {
	withSessionState(t)
	dir := ckptWorkspace(t)
	m := &Model{todos: &todoList{}}
	turns := maxUndoDepth + 5
	for i := range turns {
		f := filepath.Join(dir, string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m.snapshotBeforeMutate()
		m.finalizeCheckpoint(fmt.Sprintf("turn-%02d", i))
	}

	data, err := os.ReadFile(checkpointPath())
	if err != nil {
		t.Fatal(err)
	}
	var persisted []snapshot
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
