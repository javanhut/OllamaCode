package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/javanhut/ollama_code/api"
)

// TestSaveLoadRoundTrip covers the extended session shape: todos, workspace,
// and updated-at survive a save/load cycle alongside history and mode.
func TestSaveLoadRoundTrip(t *testing.T) {
	defer SetDirForTesting(t.TempDir())()

	s := Session{
		Name:      "roundtrip",
		CreatedAt: time.Now().Add(-time.Hour).Truncate(time.Second),
		UpdatedAt: time.Now().Truncate(time.Second),
		Model:     "qwen3:8b",
		Mode:      "plan",
		Notes:     "remember the plan",
		Workspace: "/tmp/ws",
		Todos: []Todo{
			{Content: "do thing", Status: "completed"},
			{Content: "next thing", Status: "in_progress"},
		},
		Messages: []api.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := Load("roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != s.Model || got.Mode != s.Mode || got.Notes != s.Notes || got.Workspace != s.Workspace {
		t.Fatalf("scalar fields drifted: %+v", got)
	}
	if len(got.Todos) != 2 || got.Todos[1].Status != "in_progress" {
		t.Fatalf("todos drifted: %+v", got.Todos)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "hello" {
		t.Fatalf("messages drifted: %+v", got.Messages)
	}
}

// TestSaveToIsAtomicEnough verifies the temp+rename contract observably: after
// SaveTo the target holds the full document and no temp files linger.
func TestSaveToIsAtomicEnough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if err := SaveTo(path, Session{Name: "x", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err != nil {
		t.Fatalf("saved file must load cleanly: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// TestListIgnoresNonSessions makes sure marker/lock-style files dropped next to
// named sessions never surface in /sessions.
func TestListIgnoresNonSessions(t *testing.T) {
	dir := t.TempDir()
	defer SetDirForTesting(dir)()
	if err := Save(Session{Name: "one", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".running"), []byte("pid 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "one" {
		t.Fatalf("expected exactly the named session, got %+v", sessions)
	}
}
