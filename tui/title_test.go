package tui

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// /save of a conversation with no title yet persists the deterministic
// fallback, unpinned, so the generator may still upgrade it.
func TestSaveWritesFallbackTitle(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)

	m.saveCommand("work")

	s, err := session.Load("work")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "first" {
		t.Fatalf("title = %q, want the fallback from the first user message", s.Title)
	}
	if s.TitlePinned {
		t.Fatal("a derived title must not be pinned")
	}
	if m.sessionName != "work" {
		t.Fatalf("sessionName = %q, want work", m.sessionName)
	}
}

// /title pins against the generator and writes through to the named session.
func TestTitleCommandPinsAndPersists(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)
	m.saveCommand("work")

	m.titleCommand("Fix   login bug")

	if m.sessionTitle != "Fix login bug" || !m.titlePinned {
		t.Fatalf("title = %q pinned=%v", m.sessionTitle, m.titlePinned)
	}
	s, err := session.Load("work")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Fix login bug" || !s.TitlePinned {
		t.Fatalf("saved title = %q pinned=%v", s.Title, s.TitlePinned)
	}

	// A generated title arriving after the pin must not overwrite it.
	m.applyGeneratedTitle(titleDoneMsg{title: "better title"})
	if m.sessionTitle != "Fix login bug" {
		t.Fatalf("pinned title overwritten with %q", m.sessionTitle)
	}
}

// An unpinned title takes the generated upgrade; a blank result keeps the old one.
func TestApplyGeneratedTitle(t *testing.T) {
	m := branchModel(t)
	m.applyGeneratedTitle(titleDoneMsg{title: "Async title gen"})
	if m.sessionTitle != "Async title gen" {
		t.Fatalf("title = %q", m.sessionTitle)
	}
	m.applyGeneratedTitle(titleDoneMsg{})
	if m.sessionTitle != "Async title gen" {
		t.Fatalf("empty result cleared the title: %q", m.sessionTitle)
	}
}

// /rename moves the saved session file and keeps its title; without a named
// session there is nothing to rename.
func TestRenameCommand(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)

	m.renameCommand("new-name")
	if !strings.Contains(m.toast, "/save") {
		t.Fatalf("rename without a saved session should point at /save: %q", m.toast)
	}

	m.saveCommand("work")
	m.titleCommand("kept title")
	m.renameCommand("new-name")

	if m.sessionName != "new-name" {
		t.Fatalf("sessionName = %q, want new-name", m.sessionName)
	}
	if _, err := session.Load("work"); err == nil {
		t.Fatal("old session file survived the rename")
	}
	s, err := session.Load("new-name")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "kept title" || !s.TitlePinned {
		t.Fatalf("renamed session title = %q pinned=%v", s.Title, s.TitlePinned)
	}
}

// The generator fires once: the first eligible turn latches titleGenTried, a
// pinned title or a routed provider skips the request, and a resumed
// conversation (first reply already in history) never fires at all.
func TestMaybeTitleCmdFiresOnce(t *testing.T) {
	m := branchModel(t)
	m.modelName = "testmodel"

	if cmd := m.maybeTitleCmd(); cmd == nil {
		t.Fatal("first turn should fire title generation")
	}
	if !m.titleGenTried {
		t.Fatal("firing should latch titleGenTried")
	}
	if cmd := m.maybeTitleCmd(); cmd != nil {
		t.Fatal("generation fired twice")
	}

	m2 := branchModel(t)
	m2.modelName = "testmodel"
	m2.titlePinned = true
	if cmd := m2.maybeTitleCmd(); cmd != nil {
		t.Fatal("pinned title should skip generation")
	}
}

// A routed provider is a metered API: generation skips it but still latches,
// so moving back to the local model doesn't re-fire mid-conversation.
func TestMaybeTitleCmdSkipsRoutedProvider(t *testing.T) {
	m := branchModel(t)
	m.modelName = "testmodel"
	m.cfg.Host = "http://localhost:11434"
	m.host.SetURI("https://api.example.com")

	if cmd := m.maybeTitleCmd(); cmd != nil {
		t.Fatal("generation should not run on a routed provider")
	}
	if !m.titleGenTried {
		t.Fatal("the skip should still latch the one-shot")
	}
}

// Restoring a session whose first reply already happened must not re-fire the
// one-shot generation, and its title state comes along.
func TestResumeCarriesTitleState(t *testing.T) {
	withSessionState(t)
	t.Chdir(t.TempDir())
	if err := session.SaveTo(autosavePath(), session.Session{
		Title:       "saved title",
		TitlePinned: true,
		Messages: []api.Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "a"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m := &Model{notes: &sessionNotes{}, todos: &todoList{}}
	m.restoreSession("")

	if m.sessionTitle != "saved title" || !m.titlePinned {
		t.Fatalf("title = %q pinned=%v", m.sessionTitle, m.titlePinned)
	}
	if !m.titleGenTried {
		t.Fatal("a resumed conversation with replies must not re-fire generation")
	}
}
